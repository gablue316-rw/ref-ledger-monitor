import { writeFile } from "node:fs/promises";
import { pathToFileURL } from "node:url";

export function monitorConfig(value) {
  let url;
  try {
    url = new URL(value);
  } catch {
    throw new Error("MONITOR_URL must be an HTTPS origin, for example https://monitor.example.com");
  }
  if (url.protocol !== "https:" || !url.hostname || url.username || url.password ||
      (url.pathname !== "/" && url.pathname !== "/admin/") || url.search || url.hash) {
    throw new Error("MONITOR_URL must be an HTTPS origin with no credentials, query, or fragment");
  }
  url.pathname = "/admin/";
  return {
    appId: "com.refledger.monitor",
    appName: "Ref Ledger Monitor",
    webDir: "www",
    loggingBehavior: "none",
    server: { url: url.href, cleartext: false },
  };
}

if (process.argv[1] && import.meta.url === pathToFileURL(process.argv[1]).href) {
  try {
    const config = monitorConfig(process.env.MONITOR_URL);
    await writeFile("capacitor.config.json", JSON.stringify(config, null, 2) + "\n");
    console.log(`Configured Monitor origin: ${new URL(config.server.url).origin}`);
  } catch (error) {
    console.error(error.message);
    process.exitCode = 1;
  }
}
