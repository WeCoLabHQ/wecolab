import type { APIRoute } from "astro";
import { docs, readSource, type Doc } from "../../lib/docs";

export function getStaticPaths() {
  return docs.map((doc) => ({ params: { path: doc.source }, props: { doc } }));
}

export const GET: APIRoute = ({ props }) => {
  const doc = props.doc as Doc;
  return new Response(readSource(doc), {
    headers: {
      "Content-Type": "text/plain; charset=utf-8",
      "Content-Disposition": `attachment; filename="${doc.source.split("/").pop()}"`,
    },
  });
};
