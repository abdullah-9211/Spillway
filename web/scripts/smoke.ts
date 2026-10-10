/**
 * One basic smoke check of the running stack, over HTTP with no browser: the login page loads, a wrong password
 * shows the error, a signed-out visitor is sent to /login, and each role signs in and sees its role in the
 * shell. Run with the Go service and `npm start` up: SMOKE_URL=http://localhost:3000 npm run smoke
 */
const base = process.env.SMOKE_URL ?? "http://localhost:3000";
const api = process.env.SMOKE_API ?? "http://localhost:8080";
const admin = { u: process.env.SEED_ADMIN_USER ?? "admin", p: process.env.SEED_ADMIN_PASSWORD ?? "" };
const viewer = { u: process.env.SEED_VIEWER_USER ?? "viewer", p: process.env.SEED_VIEWER_PASSWORD ?? "" };

let failed = 0;
function check(name: string, ok: boolean, detail = "") {
  console.log(`${ok ? "PASS" : "FAIL"}  ${name}${ok ? "" : "  " + detail}`);
  if (!ok) failed++;
}

async function signIn(u: string, p: string, ip: string) {
  const res = await fetch(base + "/api/auth/login", {
    method: "POST",
    headers: { "content-type": "application/json", "x-forwarded-for": ip },
    body: JSON.stringify({ username: u, password: p }),
  });
  const cookie = (res.headers.get("set-cookie") ?? "").split(";")[0];
  return { res, cookie };
}

async function main() {
const login = await fetch(base + "/login");
const loginHtml = await login.text();
check("login page loads", login.status === 200 && loginHtml.includes("Sign in") && loginHtml.includes("Username"), `status ${login.status}`);
check("no error shown before an attempt", !loginHtml.includes('role="alert"'));

const bare = await fetch(base + "/", { redirect: "manual" });
check("signed-out visitor is redirected to /login", bare.status >= 300 && bare.status < 400 && (bare.headers.get("location") ?? "").endsWith("/login"), `status ${bare.status} -> ${bare.headers.get("location")}`);

const wrong = await signIn(admin.u, "definitely-wrong", "198.51.100.1");
const wrongBody = (await wrong.res.json()) as { code?: string };
check("wrong password is refused with no cookie", wrong.res.status === 401 && wrongBody.code === "invalid_credentials" && !wrong.cookie, `status ${wrong.res.status}`);

for (const [label, who, role] of [["admin", admin, "admin"], ["viewer", viewer, "viewer"]] as const) {
  const { res, cookie } = await signIn(who.u, who.p, "198.51.100.2");
  const setCookie = res.headers.get("set-cookie") ?? "";
  check(`${label} signs in`, res.status === 200 && cookie.startsWith("spillway_session="), `status ${res.status}`);
  check(`${label} cookie is httpOnly and SameSite=Strict`, /httponly/i.test(setCookie) && /samesite=strict/i.test(setCookie), setCookie);
  const home = await fetch(base + "/", { headers: { cookie } });
  const html = await home.text();
  check(`${label} sees the shell with the "${role}" role`, home.status === 200 && html.includes("Spillway") && html.includes(`data-role="${role}"`), `status ${home.status}`);
  const keysPage = await fetch(base + "/keys", { headers: { cookie } });
  const keysHtml = await keysPage.text();
  const createDisabled = /<button[^>]*disabled[^>]*>Create key/.test(keysHtml);
  check(`${label} opens API keys`, keysPage.status === 200 && keysHtml.includes("API keys"), `status ${keysPage.status}`);
  check(
    label === "admin" ? "admin can use Create key" : "viewer's Create key is disabled and a read-only note shows",
    label === "admin" ? !createDisabled : createDisabled && keysHtml.includes("signed in as a viewer"),
  );

  const attempt = await fetch(base + "/api/keys", {
    method: "POST",
    headers: { cookie, "content-type": "application/json" },
    body: JSON.stringify({ name: "smoke-" + Date.now() }),
  });
  if (label === "viewer") {
    check("viewer cannot create a key (403)", attempt.status === 403, `status ${attempt.status}`);
  } else {
    const made = (await attempt.json()) as { key?: { id: string; prefix: string }; secret?: string };
    check("admin creates a key and gets the secret once", attempt.status === 201 && !!made.secret?.startsWith("spw_"), `status ${attempt.status}`);
    const listed = await (await fetch(base + "/keys", { headers: { cookie } })).text();
    check("the new key shows in the table by prefix only", listed.includes(made.key!.prefix) && !listed.includes(made.secret!));
    const works = await fetch(api + "/v1/models", { headers: { authorization: "Bearer " + made.secret } });
    check("the new key authenticates on the gateway", works.status === 200, `status ${works.status}`);
    const revoked = await fetch(base + "/api/keys/" + made.key!.id, { method: "DELETE", headers: { cookie } });
    check("admin revokes it", revoked.status === 200);
    const refused = await fetch(api + "/v1/models", { headers: { authorization: "Bearer " + made.secret } });
    check("a revoked key is refused on the gateway (401)", refused.status === 401, `status ${refused.status}`);
  }

  const usagePage = await fetch(base + "/usage", { headers: { cookie } });
  const usageHtml = await usagePage.text();
  check(`${label} can open Usage and cost`, usagePage.status === 200 && usageHtml.includes("Usage and cost") && usageHtml.includes("Usage over time") && usageHtml.includes("By model") && usageHtml.includes("Provider health"), `status ${usagePage.status}`);
  const week = await fetch(base + "/usage?range=7d", { headers: { cookie } });
  check(`${label} can switch the range`, week.status === 200 && (await week.text()).includes('aria-current="true"'));
  const custom = await fetch(base + "/usage?range=custom&from=2020-01-01&to=2020-01-31", { headers: { cookie } });
  const customHtml = await custom.text();
  check(`${label} can choose a custom range`, custom.status === 200 && customHtml.includes("Custom range") && customHtml.includes("2020-01-01"), `status ${custom.status}`);
  const badCustom = await fetch(base + "/usage?range=custom&from=2026-10-09&to=2026-10-01", { headers: { cookie } });
  check(`${label} sees a clear message for a backwards custom range`, badCustom.status === 200 && (await badCustom.text()).includes("The first day is after the last day"), `status ${badCustom.status}`);
  const csv = await fetch(base + "/api/usage/export?from=2020-01-01&to=2020-01-31", { headers: { cookie } });
  const csvText = await csv.text();
  check(`${label} can export a CSV`, csv.status === 200 && (csv.headers.get("content-type") ?? "").includes("text/csv") && csvText.startsWith("day,key,model,requests"), `status ${csv.status}: ${csvText.slice(0, 60)}`);

  const runsHome = await fetch(base + "/", { headers: { cookie } });
  const runsHtml = await runsHome.text();
  check(`${label} sees the Runs page with its counters`, runsHome.status === 200 && runsHtml.includes("Search runs by task") && runsHtml.includes("Succeeded") && runsHtml.includes("Time range") && runsHtml.includes("New run"), `status ${runsHome.status}`);
  const newRunDisabled = /<button[^>]*disabled[^>]*>New run</.test(runsHtml);
  check(label === "admin" ? "admin can start a run from the page" : "viewer's New run is disabled", label === "admin" ? !newRunDisabled : newRunDisabled);
  const started = await fetch(base + "/api/runs", { method: "POST", headers: { cookie, "content-type": "application/json" }, body: JSON.stringify({ input: "smoke " + Date.now() }) });
  if (label === "viewer") check("viewer cannot start a run (403)", started.status === 403, `status ${started.status}`);
  else {
    const made = (await started.json()) as { id?: string };
    check("admin starts a run from the dashboard", started.status === 202 && !!made.id, `status ${started.status}`);
    const cancelled = await fetch(base + `/api/runs/${made.id}/cancel`, { method: "POST", headers: { cookie } });
    check("admin cancels it", cancelled.status === 202 || cancelled.status === 409, `status ${cancelled.status}`);
  }
  const runsWeek = await fetch(base + "/?hours=168", { headers: { cookie } });
  check(`${label} can pick the 7 day range`, runsWeek.status === 200 && (await runsWeek.text()).includes("Activity in the last 7 days"));
  const snapRes = await fetch(base + "/api/runs/snapshot?hours=24", { headers: { cookie } });
  const snapJson = (await snapRes.json()) as { counts?: { running?: number }; active?: unknown[]; finished?: unknown[] };
  check(`${label} gets the live snapshot`, snapRes.status === 200 && typeof snapJson.counts?.running === "number" && Array.isArray(snapJson.active) && Array.isArray(snapJson.finished), `status ${snapRes.status}`);
  const noSnap = await fetch(base + "/api/runs/snapshot", { redirect: "manual" });
  check("the live snapshot needs a session", noSnap.status === 401 || (noSnap.status >= 300 && noSnap.status < 400), `status ${noSnap.status}`);
  const bad = await fetch(base + "/runs/not-a-run-id", { headers: { cookie } });
  check(`${label} gets a 404 for a run that does not exist`, bad.status === 404, `status ${bad.status}`);

  const pg = await fetch(base + "/playground", { headers: { cookie } });
  const pgHtml = await pg.text();
  const sendDisabled = /<button[^>]*disabled[^>]*>Send</.test(pgHtml);
  check(`${label} opens the Playground`, pg.status === 200 && pgHtml.includes("Routing policy") && pgHtml.includes("Recent"), `status ${pg.status}`);
  check(label === "admin" ? "admin can send a prompt" : "viewer's Send is disabled with the reason shown", label === "admin" ? !sendDisabled : sendDisabled && pgHtml.includes("only admins can"));
  const chat = await fetch(base + "/api/playground/chat", {
    method: "POST",
    headers: { cookie, "content-type": "application/json" },
    body: JSON.stringify({ policy: "default", prompt: "Say hello", faults: [{ provider: "anthropic", kind: "rate_limit" }] }),
  });
  if (label === "viewer") {
    check("viewer cannot send a playground prompt (403)", chat.status === 403, `status ${chat.status}`);
  } else {
    const r = (await chat.json()) as { attempts?: { injected?: boolean }[]; answer?: string };
    check("admin's injected 429 falls back and still answers", chat.status === 200 && !!r.answer && r.attempts?.[0]?.injected === true && (r.attempts?.length ?? 0) >= 2, `status ${chat.status}`);
  }
  const hist = await fetch(base + "/api/playground/history?limit=5", { headers: { cookie } });
  check(`${label} can read the playground history`, hist.status === 200 && Array.isArray(((await hist.json()) as { requests?: unknown[] }).requests));
}

const noCsv = await fetch(base + "/api/usage/export", { redirect: "manual" });
  check("the CSV export needs a session", noCsv.status === 401 || (noCsv.status >= 300 && noCsv.status < 400), `status ${noCsv.status}`);

  const forged = await fetch(base + "/", { headers: { cookie: "spillway_session=not.a.real.token" }, redirect: "manual" });
check("a forged session cookie is not accepted", forged.status >= 300 && forged.status < 400, `status ${forged.status}`);

  process.exit(failed ? 1 : 0);
}

main().catch((e) => {
  console.error(e);
  process.exit(1);
});
