// GitHub star count in the header. Fetched from the browser, not the server: the process makes
// no outbound requests by design, so this is the one place the page talks to another host, and
// the CSP names api.github.com for exactly this. Fails quiet: without a count the link still
// reads "GitHub". Cached an hour per browser so reloads do not eat the 60/hour anonymous limit.
(function () {
  // Earlier versions of this page had an upload form that kept the API token in localStorage.
  // The form is gone; a bearer token must not stay behind in every browser that used it.
  try { localStorage.removeItem("prunto_token"); } catch (_) {}

  const el = document.getElementById("stars");
  const link = el && el.closest("a[data-repo]");
  if (!link) return;
  const repo = link.dataset.repo;
  const key = "prunto_stars:" + repo;
  const show = (n) => {
    el.textContent = n >= 1000 ? (n / 1000).toFixed(1).replace(/\.0$/, "") + "k" : String(n);
    el.parentElement.hidden = false;
  };
  try {
    const cached = JSON.parse(localStorage.getItem(key) || "null");
    if (cached && typeof cached.n === "number" && Date.now() - cached.at < 3600e3) return show(cached.n);
  } catch (_) {}
  fetch("https://api.github.com/repos/" + repo, { headers: { Accept: "application/vnd.github+json" } })
    .then((r) => (r.ok ? r.json() : null))
    .then((d) => {
      if (!d || typeof d.stargazers_count !== "number") return;
      show(d.stargazers_count);
      try { localStorage.setItem(key, JSON.stringify({ n: d.stargazers_count, at: Date.now() })); } catch (_) {}
    })
    .catch(() => {});
})();
