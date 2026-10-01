import { readFileSync } from "node:fs";
import { resolve, posix } from "node:path";
import MarkdownIt from "markdown-it";
import anchor from "markdown-it-anchor";
import { url } from "./paths";

export type Doc = {
  slug: string;
  title: string;
  source: string;
  group: "Getting started" | "Concepts" | "Operations" | "Development";
  summary: string;
  historical?: boolean;
};

export const groups = [
  "Getting started",
  "Concepts",
  "Operations",
  "Development",
] as const;

export const docs: Doc[] = [
  {
    slug: "overview",
    title: "Overview",
    source: "README.md",
    group: "Getting started",
    summary: "What WeCoLab runs and where to start.",
  },
  {
    slug: "install",
    title: "Install a fabric",
    source: "docs/install.md",
    group: "Getting started",
    summary:
      "Prerequisites, DNS delegation, first site, and joining another site.",
  },
  {
    slug: "architecture",
    title: "Architecture",
    source: "docs/architecture.md",
    group: "Concepts",
    summary: "The fabric, its components, replication and recovery.",
  },
  {
    slug: "glossary",
    title: "Glossary",
    source: "docs/glossary.md",
    group: "Concepts",
    summary: "WeCoLab terms and their familiar equivalents.",
  },
  {
    slug: "console",
    title: "Console",
    source: "docs/console.md",
    group: "Concepts",
    summary:
      "Using the web interface to manage sites, apps, people and storage.",
  },
  {
    slug: "secrets",
    title: "Secrets",
    source: "docs/secrets.md",
    group: "Operations",
    summary: "Keys, creation, access and recovery.",
  },
  {
    slug: "operations",
    title: "Operations",
    source: "docs/operations.md",
    group: "Operations",
    summary: "Site changes, app moves, failover and rebuilds.",
  },
  {
    slug: "security",
    title: "Security",
    source: "docs/security.md",
    group: "Operations",
    summary: "Workload isolation and the limits of trust.",
  },
  {
    slug: "decisions",
    title: "Design decisions",
    source: "docs/decisions.md",
    group: "Development",
    summary: "Trade-offs and reasons to revisit them.",
  },
  {
    slug: "development",
    title: "Development",
    source: "docs/development.md",
    group: "Development",
    summary: "The source tree, local fabric and test workflow.",
  },
  {
    slug: "mac",
    title: "WeCoLab for Mac",
    source: "mac/README.md",
    group: "Development",
    summary: "Building the Apple Silicon Mac node from source.",
  },
  {
    slug: "hardening",
    title: "Hardening plan",
    source: "docs/plans/2026-09-29-hardening.md",
    group: "Development",
    summary:
      "A dated engineering plan, preserved as historical development material.",
    historical: true,
  },
];

const bySource: Record<string, Doc> = Object.fromEntries(
  docs.map((doc) => [doc.source.toLowerCase(), doc]),
);
export const docHref = (doc: Doc) => url(`docs/${doc.slug}/`);
export const rawHref = (doc: Doc) => url(`reference/${doc.source}`);
export const githubHref = (source: string) =>
  `https://github.com/WeCoLabHQ/wecolab/blob/main/${source.split("/").map(encodeURIComponent).join("/")}`;

// Render documentation from this checkout; no separate copy lives in the website.
const repoRoot = resolve(process.cwd(), process.env.WECOLAB_REPO_PATH || "..");
export const readSource = (doc: Doc) =>
  readFileSync(resolve(repoRoot, doc.source), "utf8");

function localHref(source: string, href: string): string {
  if (
    !href ||
    href.startsWith("#") ||
    href.startsWith("/") ||
    /^[a-z][a-z\d+.-]*:/i.test(href) ||
    href.startsWith("//")
  )
    return href;
  const match = /^([^?#]*)(\?[^#]*)?(#.*)?$/.exec(href);
  if (!match) return href;
  const [, pathname, query = "", fragment = ""] = match;
  if (!pathname) return href;
  let target = posix.normalize(posix.join(posix.dirname(source), pathname));
  // Some source documents use repository-root paths even when their own file is in docs/.
  if (!bySource[target.toLowerCase()] && /^(docs|mac)\//.test(pathname))
    target = posix.normalize(pathname);
  const doc = bySource[target.toLowerCase()];
  if (doc) return `${docHref(doc)}${query}${fragment}`;
  if (target.startsWith("../") || target.startsWith("/")) return href;
  return `${githubHref(target)}${query}${fragment}`;
}

export type Heading = { depth: number; title: string; id: string };
export function renderDoc(doc: Doc): { html: string; headings: Heading[] } {
  const md = new MarkdownIt({
    html: false,
    linkify: true,
    typographer: false,
  }).use(anchor, {
    slugify: (title: string) =>
      title
        .trim()
        .toLowerCase()
        .replace(/<[^>]*>/g, "")
        .replace(/[^\p{L}\p{N}\s-]/gu, "")
        .replace(/\s+/g, "-"),
  });
  const defaultLinkOpen =
    md.renderer.rules.link_open ??
    ((tokens, idx, options, _env, self) =>
      self.renderToken(tokens, idx, options));
  md.renderer.rules.link_open = (tokens, idx, options, env, self) => {
    const token = tokens[idx];
    const href = token.attrGet("href");
    if (href) {
      const rewritten = localHref(doc.source, href);
      token.attrSet("href", rewritten);
      if (rewritten.startsWith("https://github.com/"))
        token.attrSet("rel", "noopener noreferrer");
    }
    return defaultLinkOpen(tokens, idx, options, env, self);
  };
  const tokens = md.parse(readSource(doc), {});
  const headings: Heading[] = [];
  for (let i = 0; i < tokens.length; i++) {
    if (tokens[i].type !== "heading_open") continue;
    const depth = Number(tokens[i].tag.slice(1));
    if (depth >= 2 && depth <= 3)
      headings.push({
        depth,
        title: tokens[i + 1].content,
        id: tokens[i].attrGet("id") || "",
      });
    if (
      depth === 1 &&
      !tokens.slice(0, i).some((token) => token.type === "heading_open")
    ) {
      // The page supplies its own H1 above the original source body.
      tokens.splice(i, 3);
      i--;
    }
  }
  return { html: md.renderer.render(tokens, md.options, {}), headings };
}
