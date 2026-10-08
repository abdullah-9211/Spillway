// Fails when src/lib/api-types.ts no longer matches ../api/openapi.yaml.
import { execFileSync } from "node:child_process";
import fs from "node:fs";
import os from "node:os";
import path from "node:path";

const tmp = path.join(fs.mkdtempSync(path.join(os.tmpdir(), "spillway-types-")), "api-types.ts");
execFileSync("npx", ["openapi-typescript", "../api/openapi.yaml", "-o", tmp], { stdio: "ignore" });
const fresh = fs.readFileSync(tmp, "utf8");
const current = fs.existsSync("src/lib/api-types.ts") ? fs.readFileSync("src/lib/api-types.ts", "utf8") : "";
if (fresh !== current) {
  console.error("src/lib/api-types.ts is out of date: run `npm run types`");
  process.exit(1);
}
console.log("types: up to date");
