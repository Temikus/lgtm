const APEX = "getlgtm.dev";
// Deliberately loose: one @, a dot in the domain, no spaces.
const EMAIL = /^[^\s@]+@[^\s@]+\.[^\s@]+$/;
const MAX_EMAIL = 254;

// Every response, assets included. The pages use no inline script or style, so the CSP stays strict.
const SECURITY_HEADERS: Record<string, string> = {
  "strict-transport-security": "max-age=31536000; includeSubDomains",
  "content-security-policy":
    "default-src 'self'; img-src 'self' data:; object-src 'none'; base-uri 'none'; form-action 'self'; frame-ancestors 'none'",
  "x-content-type-options": "nosniff",
  "referrer-policy": "strict-origin-when-cross-origin",
};

export default {
  async fetch(request, env): Promise<Response> {
    return secure(await route(request, env));
  },
} satisfies ExportedHandler<Env>;

async function route(request: Request, env: Env): Promise<Response> {
  const url = new URL(request.url);
  if (url.hostname === `www.${APEX}`) {
    url.hostname = APEX;
    return Response.redirect(url.toString(), 301);
  }
  if (url.pathname === "/api/waitlist") return waitlist(request, env, url);
  if (url.pathname.startsWith("/api/")) return new Response("Not found", { status: 404 });
  return env.ASSETS.fetch(request);
}

function secure(response: Response): Response {
  const res = new Response(response.body, response);
  for (const [name, value] of Object.entries(SECURITY_HEADERS)) res.headers.set(name, value);
  return res;
}

async function waitlist(request: Request, env: Env, url: URL): Promise<Response> {
  if (request.method !== "POST") return new Response("Method not allowed", { status: 405, headers: { allow: "POST" } });

  const ip = request.headers.get("cf-connecting-ip") ?? "unknown";
  const { success } = await env.WAITLIST_LIMIT.limit({ key: ip });
  if (!success) return new Response("Too many requests", { status: 429, headers: { "retry-after": "60" } });

  // JS clients send JSON and get JSON back; a plain form post gets a redirect.
  const isJSON = request.headers.get("content-type")?.includes("application/json") ?? false;
  const signup = await parse(request, isJSON);
  const valid = signup !== null && signup.email.length <= MAX_EMAIL && EMAIL.test(signup.email);

  // A known email gets the same answer as a new one, so the form can't be used to probe the list.
  if (valid && !signup.honeypot) {
    await env.DB.prepare("INSERT OR IGNORE INTO signups (email, created_at) VALUES (?, ?)")
      .bind(signup.email, new Date().toISOString())
      .run();
  }

  if (isJSON) return Response.json(valid ? { ok: true } : { ok: false, error: "invalid_email" }, { status: valid ? 200 : 400 });
  return Response.redirect(valid ? `${url.origin}/joined` : `${url.origin}/?invalid=1#cloud`, 303);
}

async function parse(request: Request, isJSON: boolean): Promise<{ email: string; honeypot: boolean } | null> {
  let fields: Record<string, unknown>;
  try {
    fields = isJSON ? await request.json() : Object.fromEntries(await request.formData());
  } catch {
    return null;
  }
  if (typeof fields !== "object" || fields === null || typeof fields.email !== "string") return null;
  return { email: fields.email.trim().toLowerCase(), honeypot: Boolean(fields.company) };
}
