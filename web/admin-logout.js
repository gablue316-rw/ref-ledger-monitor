"use strict";
(() => {
  const originalFetch = window.fetch.bind(window);
  window.fetch = async (...args) => {
    const response = await originalFetch(...args);
    if (response.status === 401) {
      const target = new URL(typeof args[0] === "string" ? args[0] : args[0]?.url || String(args[0]), location.href);
      const apiBase = new URL(adminURL("api/"));
      if (target.origin === apiBase.origin && target.pathname.startsWith(apiBase.pathname)) {
        location.replace(adminURL("login"));
      }
    }
    return response;
  };
  const button = document.getElementById("adminLogout");
  if (!button) return;
  button.addEventListener("click", async () => {
    button.disabled = true;
    try {
      const response = await fetch(adminURL("api/admin/logout"), {
        method: "POST", credentials: "same-origin", cache: "no-store",
        headers: { "Content-Type": "application/json", "X-Ref-Ledger-Admin": "1" },
        body: "{}", signal: AbortSignal.timeout(20000),
      });
      if (!response.ok) throw Error("Logout failed. Please try again.");
      location.replace(adminURL("login"));
    } catch (error) {
      const notice = document.getElementById("adminNotice");
      notice.hidden = false;
      notice.textContent = error.message;
      notice.style.color = "#a32020";
      button.disabled = false;
    }
  });
  // Refreshing a page restored by Back must recheck its server session.
  window.addEventListener("pageshow", (event) => {
    if (event.persisted) location.reload();
  });
})();
