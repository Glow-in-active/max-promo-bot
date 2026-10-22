import { buildSimulator, loadConfig } from "./server.mjs";
import { buildBannerServer } from "./banner.mjs";

const app = buildSimulator(loadConfig(process.env));
let banner;

async function close() {
  await Promise.all([
    app.close(),
    banner?.listening ? new Promise((resolve, reject) => banner.close(error => error ? reject(error) : resolve())) : Promise.resolve(),
  ]);
}

try {
  banner = await buildBannerServer();
  await new Promise((resolve, reject) => {
    banner.once("error", reject);
    banner.listen(4011, "0.0.0.0", resolve);
  });
  await app.listen({ host: "0.0.0.0", port: 4010 });
  console.log(JSON.stringify({ level: "info", message: "MAX simulator listening", port: 4010, banner_port: 4011 }));
} catch {
  await close().catch(() => {});
  console.error(JSON.stringify({ level: "error", message: "MAX simulator startup failed" }));
  process.exit(1);
}

for (const signal of ["SIGTERM", "SIGINT"]) {
  process.once(signal, () => {
    close().catch(() => { process.exitCode = 1; });
  });
}
