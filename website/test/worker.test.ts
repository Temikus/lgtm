import { SELF } from "cloudflare:test";
import { env } from "cloudflare:workers";
import { beforeEach, describe, expect, it } from "vitest";

const ORIGIN = "https://getlgtm.dev";

// The rate limiter keys on the client IP, so each request gets its own unless a test shares one.
let nextIP = 0;
function freshIP() {
  nextIP++;
  return `198.51.100.${nextIP % 250}`;
}

function postJSON(body: unknown, ip = freshIP()) {
  return SELF.fetch(`${ORIGIN}/api/waitlist`, {
    method: "POST",
    headers: { "content-type": "application/json", "cf-connecting-ip": ip },
    body: JSON.stringify(body),
  });
}

function postForm(fields: Record<string, string>) {
  return SELF.fetch(`${ORIGIN}/api/waitlist`, {
    method: "POST",
    headers: { "cf-connecting-ip": freshIP() },
    body: new URLSearchParams(fields),
    redirect: "manual",
  });
}

async function storedEmails() {
  const { results } = await env.DB.prepare("SELECT email FROM signups ORDER BY email").all();
  return results.map((r) => r.email);
}

describe("POST /api/waitlist", () => {
  beforeEach(async () => {
    await env.DB.prepare("DELETE FROM signups").run();
  });

  it("stores a valid signup, normalised", async () => {
    const res = await postJSON({ email: "  Dev@Example.com " });
    expect(res.status).toBe(200);
    expect(await res.json()).toEqual({ ok: true });
    expect(await storedEmails()).toEqual(["dev@example.com"]);
  });

  it("answers a duplicate exactly like a new signup", async () => {
    const first = await postJSON({ email: "dev@example.com" });
    const again = await postJSON({ email: "dev@example.com" });
    expect(again.status).toBe(first.status);
    expect(await again.text()).toBe(await first.text());
    expect(await storedEmails()).toEqual(["dev@example.com"]);
  });

  it.each(["", "not-an-email", "a@b", "has space@example.com", "x".repeat(243) + "@example.com"])("rejects %j", async (email) => {
    const res = await postJSON({ email });
    expect(res.status).toBe(400);
    expect(await storedEmails()).toEqual([]);
  });

  it("accepts an email at the length cap", async () => {
    const email = "x".repeat(242) + "@example.com";
    expect(email.length).toBe(254);
    expect((await postJSON({ email })).status).toBe(200);
  });

  it.each([null, [], { email: 42 }, "dev@example.com"])("rejects body %j", async (body) => {
    const res = await postJSON(body);
    expect(res.status).toBe(400);
  });

  it("rejects malformed JSON", async () => {
    const res = await SELF.fetch(`${ORIGIN}/api/waitlist`, {
      method: "POST",
      headers: { "content-type": "application/json", "cf-connecting-ip": freshIP() },
      body: "{",
    });
    expect(res.status).toBe(400);
  });

  it("pretends success when the honeypot is filled", async () => {
    const res = await postJSON({ email: "bot@example.com", company: "Acme" });
    expect(res.status).toBe(200);
    expect(await storedEmails()).toEqual([]);
  });

  it("redirects a no-JS form post to the confirmed page", async () => {
    const res = await postForm({ email: "dev@example.com", company: "" });
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(`${ORIGIN}/joined`);
    expect(await storedEmails()).toEqual(["dev@example.com"]);
  });

  it("redirects an invalid no-JS form post back with an error flag", async () => {
    const res = await postForm({ email: "nope" });
    expect(res.status).toBe(303);
    expect(res.headers.get("location")).toBe(`${ORIGIN}/?invalid=1#cloud`);
  });

  it("rate-limits one client after five posts a minute", async () => {
    const ip = "203.0.113.7";
    for (let i = 0; i < 5; i++) expect((await postJSON({ email: `dev${i}@example.com` }, ip)).status).toBe(200);
    const res = await postJSON({ email: "dev5@example.com" }, ip);
    expect(res.status).toBe(429);
    expect(await storedEmails()).not.toContain("dev5@example.com");
    expect((await postJSON({ email: "other@example.com" })).status).toBe(200);
  });

  it("rejects other methods", async () => {
    const res = await SELF.fetch(`${ORIGIN}/api/waitlist`);
    expect(res.status).toBe(405);
  });
});

describe("routing", () => {
  it("serves the landing page", async () => {
    const res = await SELF.fetch(`${ORIGIN}/`);
    expect(res.status).toBe(200);
    const html = await res.text();
    expect(html).toContain("Know which PRs");
    expect(html).toContain('action="/api/waitlist"');
  });

  it.each(["/joined", "/privacy"])("serves %s", async (path) => {
    const res = await SELF.fetch(`${ORIGIN}${path}`);
    expect(res.status).toBe(200);
  });

  it("redirects www to the apex domain", async () => {
    const res = await SELF.fetch("https://www.getlgtm.dev/install?x=1", { redirect: "manual" });
    expect(res.status).toBe(301);
    expect(res.headers.get("location")).toBe("https://getlgtm.dev/install?x=1");
  });

  it("404s unknown API routes", async () => {
    const res = await SELF.fetch(`${ORIGIN}/api/nope`, { method: "POST" });
    expect(res.status).toBe(404);
  });
});

describe("security headers", () => {
  it.each([
    ["page", () => SELF.fetch(`${ORIGIN}/`)],
    ["asset", () => SELF.fetch(`${ORIGIN}/styles.css`)],
    ["redirect", () => SELF.fetch("https://www.getlgtm.dev/", { redirect: "manual" })],
    ["api", () => postJSON({ email: "dev@example.com" })],
    ["404", () => SELF.fetch(`${ORIGIN}/api/nope`)],
  ])("sets HSTS and a CSP on a %s response", async (_, fetch) => {
    const res = await fetch();
    expect(res.headers.get("strict-transport-security")).toMatch(/^max-age=\d{8}/);
    expect(res.headers.get("content-security-policy")).toContain("default-src 'self'");
    expect(res.headers.get("content-security-policy")).toContain("frame-ancestors 'none'");
    expect(res.headers.get("x-content-type-options")).toBe("nosniff");
  });

  it("keeps pages free of inline scripts and style attributes, which the CSP blocks", async () => {
    for (const path of ["/", "/joined", "/privacy"]) {
      const html = await (await SELF.fetch(`${ORIGIN}${path}`)).text();
      expect(html, path).not.toMatch(/<script(?![^>]*\bsrc=)/);
      expect(html, path).not.toMatch(/\sstyle=/);
      expect(html, path).not.toMatch(/\son[a-z]+=/);
    }
  });
});
