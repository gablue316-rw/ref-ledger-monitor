package main

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
)

const adminCookie = "rl_admin_session"
const adminSessionTTL = 30 * time.Minute

func adminTokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

func (app *Application) adminCookieSecure(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(app.config.Environment, "production") ||
		strings.HasPrefix(r.Header.Get("Origin"), "https://")
}

func (app *Application) setAdminCookie(w http.ResponseWriter, r *http.Request, token string, expires time.Time) {
	maxAge := int(adminSessionTTL.Seconds())
	if token == "" {
		maxAge = -1
		expires = time.Unix(1, 0)
	}
	http.SetCookie(w, &http.Cookie{Name: adminCookie, Value: token, Path: "/",
		HttpOnly: true, Secure: app.adminCookieSecure(r), SameSite: http.SameSiteStrictMode,
		Expires: expires, MaxAge: maxAge})
}

// Authentication accepts only the admin session cookie. Cached Basic Auth
// credentials cannot silently sign the browser back in after logout.
func (app *Application) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		w.Header().Set("X-Content-Type-Options", "nosniff")
		cookie, err := r.Cookie(adminCookie)
		if err == nil && len(cookie.Value) == 64 {
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			var session struct {
				Username string `bson:"username"`
			}
			err = app.db.Collection("admin_sessions").FindOne(ctx, bson.M{
				"_id": adminTokenHash(cookie.Value), "expiresAt": bson.M{"$gt": time.Now().UTC()},
			}).Decode(&session)
			if err == nil && session.Username == app.config.Username {
				next(w, r)
				return
			}
			if err != nil && !errors.Is(err, mongo.ErrNoDocuments) {
				http.Error(w, "Administration authentication is temporarily unavailable", http.StatusServiceUnavailable)
				return
			}
		}
		if strings.HasPrefix(r.URL.Path, "/api/") {
			writeJSON(w, http.StatusUnauthorized, map[string]string{"message": "Administrator sign-in required"})
			return
		}
		http.Redirect(w, r, "/admin/login", http.StatusSeeOther)
	}
}

func (app *Application) adminLoginHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", 405)
		return
	}
	var credentials struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if !decodeAdminJSON(w, r, &credentials) {
		return
	}
	user := sha256.Sum256([]byte(credentials.Username))
	pass := sha256.Sum256([]byte(credentials.Password))
	wantUser := sha256.Sum256([]byte(app.config.Username))
	wantPass := sha256.Sum256([]byte(app.config.Password))
	validUser := subtle.ConstantTimeCompare(user[:], wantUser[:])
	validPass := subtle.ConstantTimeCompare(pass[:], wantPass[:])
	if validUser&validPass != 1 {
		writeJSON(w, 401, map[string]string{"message": "Invalid username or password."})
		return
	}
	var random [32]byte
	if _, err := rand.Read(random[:]); err != nil {
		http.Error(w, "Unable to create session", 500)
		return
	}
	token := hex.EncodeToString(random[:])
	expires := time.Now().UTC().Add(adminSessionTTL)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	sessions := app.db.Collection("admin_sessions")
	// Remove expired records without depending on a MongoDB TTL index.
	if _, err := sessions.DeleteMany(ctx, bson.M{"expiresAt": bson.M{"$lte": time.Now().UTC()}}); err != nil {
		http.Error(w, "Unable to create session", 503)
		return
	}
	// Replace this browser's prior session after a successful sign-in.
	if old, err := r.Cookie(adminCookie); err == nil && len(old.Value) == 64 {
		if _, err := sessions.DeleteOne(ctx, bson.M{"_id": adminTokenHash(old.Value)}); err != nil {
			http.Error(w, "Unable to replace session", 503)
			return
		}
	}
	if _, err := sessions.InsertOne(ctx, bson.M{"_id": adminTokenHash(token), "username": app.config.Username, "expiresAt": expires}); err != nil {
		http.Error(w, "Unable to create session", 503)
		return
	}
	app.setAdminCookie(w, r, token, expires)
	writeJSON(w, 200, map[string]string{"message": "Signed in"})
}

func (app *Application) adminLogoutHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		http.Error(w, "Method not allowed", 405)
		return
	}
	if cookie, err := r.Cookie(adminCookie); err == nil && len(cookie.Value) == 64 {
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		if _, err := app.db.Collection("admin_sessions").DeleteOne(ctx, bson.M{"_id": adminTokenHash(cookie.Value)}); err != nil {
			writeJSON(w, 503, map[string]string{"message": "Logout failed. Please try again."})
			return
		}
	}
	app.setAdminCookie(w, r, "", time.Time{})
	writeJSON(w, 200, map[string]string{"message": "Signed out"})
}

func (app *Application) adminLoginPage(w http.ResponseWriter, r *http.Request) {
	if !allowRead(w, r) {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if r.Method == http.MethodHead {
		return
	}
	_, _ = w.Write([]byte(adminLoginHTML))
}

const adminLoginHTML = `<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Ref Ledger | Administrator sign in</title><style>
*{box-sizing:border-box}body{margin:0;min-height:100dvh;display:grid;place-items:center;background:#f3f6f9;color:#192c40;font:16px/1.5 system-ui,sans-serif;padding:24px}
main{width:100%;max-width:440px;background:white;border:1px solid #dfe6ed;border-radius:14px;overflow:hidden;box-shadow:0 12px 40px #122a3c12}
.brand{background:#122a3c;color:white;padding:28px 32px;font-size:27px;font-weight:750}.brand span{color:#71c7a5}.brand small{display:block;font-size:12px;letter-spacing:2px;font-weight:400;margin-top:4px}
form{padding:32px}h1{font-size:24px;margin:0 0 8px}p{color:#647589}label{display:block;margin:20px 0 6px}input,button{font:inherit;width:100%;padding:12px;border-radius:7px;border:1px solid #ced8e2}button{background:#1f614d;color:white;border:0;margin-top:24px;cursor:pointer}button:disabled{opacity:.6}input:focus-visible,button:focus-visible{outline:3px solid #60a5fa;outline-offset:3px}#message{color:#a32020;min-height:24px}
</style></head><body><main><div class="brand">Ref <span>Ledger</span><small>ADMINISTRATION</small></div>
<form id="login"><h1>Administrator sign in</h1><p>Sign in to manage Ref Ledger.</p>
<label for="username">Username</label><input id="username" name="username" autocomplete="username" required maxlength="254">
<label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" required>
<button id="submit">Sign in</button><p id="message" role="alert"></p></form></main><script>
document.getElementById('login').onsubmit=async(event)=>{event.preventDefault();const button=document.getElementById('submit'),message=document.getElementById('message');button.disabled=true;message.textContent='';try{const response=await fetch('/admin/api/admin/login',{method:'POST',credentials:'same-origin',headers:{'Content-Type':'application/json','X-Ref-Ledger-Admin':'1'},body:JSON.stringify({username:document.getElementById('username').value,password:document.getElementById('password').value}),signal:AbortSignal.timeout(20000)});if(!response.ok){const data=await response.json().catch(()=>({}));throw Error(data.message||'Unable to sign in. Please try again.')}location.replace('/admin/')}catch(error){message.textContent=error.message;document.getElementById('password').value=''}finally{button.disabled=false}};
</script></body></html>`
