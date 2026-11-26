package com.refledger.monitor;

import android.net.Uri;
import android.os.Bundle;
import android.webkit.WebResourceRequest;
import android.webkit.WebView;
import com.getcapacitor.BridgeActivity;
import com.getcapacitor.BridgeWebViewClient;

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        if (bridge == null) return;
        Uri server = Uri.parse(bridge.getServerUrl());
        bridge.setWebViewClient(new BridgeWebViewClient(bridge) {
            private boolean blocked(Uri url) {
                return !"https".equalsIgnoreCase(url.getScheme())
                    || !server.getHost().equalsIgnoreCase(url.getHost())
                    || effectivePort(server) != effectivePort(url);
            }

            @Override
            public boolean shouldOverrideUrlLoading(WebView view, WebResourceRequest request) {
                return blocked(request.getUrl()) || super.shouldOverrideUrlLoading(view, request);
            }

            @Override
            public boolean shouldOverrideUrlLoading(WebView view, String url) {
                return blocked(Uri.parse(url)) || super.shouldOverrideUrlLoading(view, url);
            }
        });
        bridge.getWebView().getSettings().setCacheMode(android.webkit.WebSettings.LOAD_NO_CACHE);
    }

    private static int effectivePort(Uri uri) {
        return uri.getPort() == -1 ? 443 : uri.getPort();
    }

    @Override
    public void onPause() {
        if (bridge != null) {
            bridge.getWebView().evaluateJavascript("window.dispatchEvent(new Event('monitor-app-pause'))", null);
            bridge.getWebView().onPause();
        }
        super.onPause();
    }

    @Override
    public void onResume() {
        super.onResume();
        if (bridge != null) {
            bridge.getWebView().onResume();
            bridge.getWebView().evaluateJavascript("window.dispatchEvent(new Event('monitor-app-resume'))", null);
        }
    }

    @Override
    public void onBackPressed() {
        if (bridge == null) {
            super.onBackPressed();
            return;
        }
        WebView view = bridge.getWebView();
        view.evaluateJavascript("(function(){const d=document.querySelector('dialog[open]');if(d){d.close();return true}return false})()", result -> {
            if ("true".equals(result)) return;
            if (view.canGoBack()) view.goBack();
            else finish();
        });
    }
}
