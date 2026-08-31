// Drop page. Uploads straight to the API with the token the browser is holding; the token is
// never sent anywhere else and never leaves localStorage.
(function () {
  const $ = (id) => document.getElementById(id);
  const token = $("token"), drop = $("drop"), file = $("file"), hint = $("hint");
  const result = $("result"), preview = $("preview"), videoPreview = $("videoPreview");
  const markdown = $("markdown"), expiry = $("expiry"), error = $("error");
  let deleteURL = null;

  token.value = localStorage.getItem("priito_token") || "";
  token.addEventListener("input", () => localStorage.setItem("priito_token", token.value.trim()));

  const fail = (message) => { error.textContent = message; error.hidden = false; hint.textContent = "Drop a file here, paste, or click to choose one"; };

  async function upload(blob, name) {
    if (!token.value.trim()) return fail("Add an API token first.");
    error.hidden = true;
    hint.textContent = "Uploading…";

    const form = new FormData();
    form.append("file", blob, name || "upload");

    let response;
    try {
      response = await fetch("/api/v1/uploads", {
        method: "POST",
        headers: { Authorization: "Bearer " + token.value.trim() },
        body: form,
      });
    } catch (e) {
      return fail("The upload could not be sent.");
    }

    let payload = {};
    try { payload = await response.json(); } catch (e) { /* an error page, not JSON */ }
    if (!response.ok) return fail(payload.error || "Upload failed (" + response.status + ").");

    hint.textContent = "Drop another file, paste, or click to choose one";
    deleteURL = payload.delete_url;
    markdown.value = payload.markdown;

    const isVideo = (payload.content_type || "").startsWith("video/");
    preview.hidden = isVideo;
    videoPreview.hidden = !isVideo;
    if (isVideo) videoPreview.src = payload.url; else preview.src = payload.url;

    expiry.textContent = "Expires " + new Date(payload.expires_at).toLocaleString();
    result.hidden = false;
  }

  drop.addEventListener("dragover", (e) => { e.preventDefault(); drop.classList.add("over"); });
  drop.addEventListener("dragleave", () => drop.classList.remove("over"));
  drop.addEventListener("drop", (e) => {
    e.preventDefault();
    drop.classList.remove("over");
    const f = e.dataTransfer.files[0];
    if (f) upload(f, f.name);
  });
  file.addEventListener("change", () => { const f = file.files[0]; if (f) upload(f, f.name); });
  document.addEventListener("paste", (e) => {
    for (const item of e.clipboardData.items) {
      const f = item.getAsFile();
      if (f) { upload(f, f.name); return; }
    }
  });

  $("copy").addEventListener("click", async () => {
    await navigator.clipboard.writeText(markdown.value);
    $("copy").textContent = "Copied";
    setTimeout(() => ($("copy").textContent = "Copy markdown"), 1200);
  });

  $("remove").addEventListener("click", async () => {
    if (!deleteURL) return;
    await fetch(deleteURL, {
      method: "DELETE",
      headers: { Authorization: "Bearer " + token.value.trim() },
    });
    result.hidden = true;
    deleteURL = null;
  });
})();
