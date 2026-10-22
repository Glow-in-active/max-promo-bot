import { readFile } from "node:fs/promises";
import { createServer } from "node:https";
import { resolve } from "node:path";
import { fileURLToPath } from "node:url";

export const bannerPath = "/assets/banner.png";
export const bannerURL = `https://simulator:4011${bannerPath}`;
export const tlsDirectory = fileURLToPath(new URL("../tls/", import.meta.url));

export async function buildBannerServer(config = {}) {
  const directory = config.tlsDirectory ?? tlsDirectory;
  const [key, cert, image] = await Promise.all([
    readFile(resolve(directory, "key.pem")),
    readFile(resolve(directory, "cert.pem")),
    readFile(resolve(config.publicDirectory ?? fileURLToPath(new URL("../public/", import.meta.url)), "banner.png")),
  ]);
  return createServer({ key, cert, minVersion: "TLSv1.2" }, (request, response) => {
    if (request.url !== bannerPath) {
      response.writeHead(404, { "content-type": "application/json" });
      return response.end('{"error":"Asset not found"}');
    }
    if (request.method !== "GET" && request.method !== "HEAD") {
      response.writeHead(405, { "content-type": "application/json", allow: "GET, HEAD" });
      return response.end('{"error":"Method not allowed"}');
    }
    response.writeHead(200, {
      "content-type": "image/png", "content-length": image.length,
      "x-content-type-options": "nosniff", "cache-control": "no-cache",
    });
    response.end(request.method === "HEAD" ? undefined : image);
  });
}
