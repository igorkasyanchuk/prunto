// Copy-to-clipboard for the one-time token flash. Loaded as a file rather than inlined
// because the page's CSP is default-src 'self' with no unsafe-inline.
(function () {
  const button = document.getElementById("copy-token");
  if (!button) return; // no token was just created; nothing to wire up
  const field = document.getElementById(button.dataset.copyTarget);
  if (!field) return;

  let restore;
  const say = (text, ok) => {
    button.textContent = text;
    button.classList.toggle("copied", ok);
    clearTimeout(restore);
    restore = setTimeout(() => {
      button.textContent = "Copy";
      button.classList.remove("copied");
    }, 2000);
  };

  button.addEventListener("click", async () => {
    // Select either way: it is the visible confirmation that something happened, and it
    // leaves the value ready for a manual copy if the write below is refused.
    field.focus();
    field.select();
    try {
      // navigator.clipboard needs a secure context, which an instance served over plain
      // http on a LAN address is not. execCommand is deprecated but still the only
      // fallback that works there.
      if (navigator.clipboard && window.isSecureContext) {
        await navigator.clipboard.writeText(field.value);
      } else if (!document.execCommand("copy")) {
        throw new Error("execCommand refused");
      }
      say("Copied", true);
    } catch {
      // The value is selected at this point, so the manual copy is one keystroke;
      // naming the key would be wrong on half the platforms that reach this branch.
      say("Selected \u2014 copy it", false);
    }
  });
})();
