import { cpSync, existsSync, mkdirSync, readdirSync, rmSync } from "node:fs";
import { fileURLToPath } from "node:url";
const source = fileURLToPath(new URL("../web/out/", import.meta.url));
const target = fileURLToPath(new URL("../internal/web/dist/", import.meta.url));
if (!existsSync(source + "/index.html"))
  throw new Error("Run pnpm run build in web first");
mkdirSync(target, { recursive: true });
for (const entry of readdirSync(target))
  if (entry !== ".gitkeep")
    rmSync(target + "/" + entry, { recursive: true, force: true });
cpSync(source, target, { recursive: true });
console.log("Next.js static export copied into Go embed assets.");
