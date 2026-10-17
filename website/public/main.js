// Without JS the form still posts and the Worker redirects to /joined.
const JOINED_KEY = "lgtm-joined";
const INVALID = "That email doesn't look right. Try again?";

function joinedBefore() {
  try {
    return localStorage.getItem(JOINED_KEY) === "1";
  } catch {
    return false;
  }
}

function remember() {
  try {
    localStorage.setItem(JOINED_KEY, "1");
  } catch {}
}

function setupSignup(signup) {
  const form = signup.querySelector("form");
  const note = signup.querySelector(".signup-note");
  const done = () => {
    form.hidden = true;
    signup.querySelector(".signup-done").hidden = false;
  };
  const fail = (message) => {
    note.textContent = message;
    note.classList.add("signup-error");
  };

  if (joinedBefore()) done();
  if (new URLSearchParams(location.search).has("invalid")) fail(INVALID);

  form.addEventListener("submit", async (event) => {
    event.preventDefault();
    const button = form.querySelector("button");
    button.disabled = true;
    try {
      const res = await fetch(form.action, {
        method: "POST",
        headers: { "content-type": "application/json" },
        body: JSON.stringify(Object.fromEntries(new FormData(form))),
      });
      if (res.status === 400) return fail(INVALID);
      if (res.status === 429) return fail("Too many tries. Wait a minute and try again.");
      if (!res.ok) throw new Error(`HTTP ${res.status}`);
      remember();
      done();
    } catch {
      fail("Something went wrong. Try again in a minute.");
    } finally {
      button.disabled = false;
    }
  });
}

function setupCopy(button) {
  const code = document.getElementById(button.dataset.copy);
  if (!code || !navigator.clipboard) return;
  button.hidden = false;
  button.addEventListener("click", async () => {
    try {
      await navigator.clipboard.writeText(code.textContent + "\n");
      button.textContent = "Copied";
    } catch {
      button.textContent = "Select and copy";
    }
    setTimeout(() => (button.textContent = "Copy"), 2000);
  });
}

document.querySelectorAll(".signup").forEach(setupSignup);
document.querySelectorAll("[data-copy]").forEach(setupCopy);
