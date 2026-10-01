import type { APIRoute } from "astro";
import { existsSync, readdirSync, readFileSync } from "node:fs";
import { resolve } from "node:path";

// The pictures in docs/images, served as they are: the docs and README show them on GitHub, and the
// rendered docs point here (renderDoc).
const dir = resolve(process.cwd(), process.env.WECOLAB_REPO_PATH || "..", "docs/images");
const types: Record<string, string> = { png: "image/png", jpg: "image/jpeg", webp: "image/webp", svg: "image/svg+xml" };

export function getStaticPaths() {
  return (existsSync(dir) ? readdirSync(dir) : [])
    .filter((name) => types[name.split(".").pop() || ""])
    .map((name) => ({ params: { name } }));
}

export const GET: APIRoute = ({ params }) =>
  new Response(readFileSync(resolve(dir, params.name as string)), {
    headers: { "Content-Type": types[(params.name as string).split(".").pop() || ""] },
  });
