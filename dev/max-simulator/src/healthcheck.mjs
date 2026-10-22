import { readFile } from "node:fs/promises";
import { request } from "node:https";
import { resolve } from "node:path";
import { bannerPath, tlsDirectory } from "./banner.mjs";

try {
  const ca = await readFile(resolve(tlsDirectory, "cert.pem"));
  await Promise.all([
    fetch("http://127.0.0.1:4010/health", { signal: AbortSignal.timeout(2000) }).then(async response => {
      if (!response.ok || (await response.json()).ok !== true) throw new Error("HTTP health check failed");
    }),
    new Promise((resolve, reject) => {
      const probe = request({ hostname: "127.0.0.1", port: 4011, servername: "simulator", path: bannerPath, ca }, response => {
        const chunks = [];
        let length = 0;
        response.on("data", chunk => {
          length += chunk.length;
          if (length > 1 << 20) return probe.destroy(new Error("Banner is too large"));
          chunks.push(chunk);
        });
        response.on("error", reject);
        response.on("end", () => {
          const image = Buffer.concat(chunks);
          if (response.statusCode !== 200 || response.headers["content-type"] !== "image/png" || image.length < 24 || image.toString("hex", 0, 8) !== "89504e470d0a1a0a" || image.readUInt32BE(16) !== 1200 || image.readUInt32BE(20) !== 480) {
            return reject(new Error("HTTPS banner check failed"));
          }
          resolve();
        });
      });
      probe.setTimeout(2000, () => probe.destroy(new Error("HTTPS health check timed out")));
      probe.on("error", reject);
      probe.end();
    }),
  ]);
} catch {
  process.exitCode = 1;
}
