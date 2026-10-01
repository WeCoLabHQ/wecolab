export const base = import.meta.env.BASE_URL.replace(/\/$/, "");
export const url = (path = "") => `${base}/${path.replace(/^\//, "")}`;
