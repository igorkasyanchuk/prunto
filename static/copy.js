// Copy buttons. Any <button data-copy-target="some-id"> copies the text of that element -
// its value if it is a field, its text if it is a <pre>. Loaded as a file rather than inlined
// because the pages run under default-src 'self' with no unsafe-inline.
(function () {
  const text = (el) => ("value" in el ? el.value : el.textContent.trim());

  const select = (el) => {
    if ("select" in el) {
      el.focus();
      el.select();
      return;
    }
    const range = document.createRange();
    range.selectNodeContents(el);
    const sel = window.getSelection();
    sel.removeAllRanges();
    sel.addRange(range);
  };

  for (const button of document.querySelectorAll("button[data-copy-target]")) {
    const target = document.getElementById(button.dataset.copyTarget);
    if (!target) continue;

    const label = button.textContent;
    let restore;
    const say = (message, ok) => {
      button.textContent = message;
      button.classList.toggle("copied", ok);
      clearTimeout(restore);
      restore = setTimeout(() => {
        button.textContent = label;
        button.classList.remove("copied");
      }, 2000);
    };

    button.addEventListener("click", async () => {
      // Select either way: it is the visible confirmation that something happened, and it
      // leaves the text ready for a manual copy if the write below is refused.
      select(target);
      try {
        // navigator.clipboard needs a secure context, which an instance served over plain
        // http on a LAN address is not. execCommand is deprecated but still the only
        // fallback that works there.
        if (navigator.clipboard && window.isSecureContext) {
          await navigator.clipboard.writeText(text(target));
        } else if (!document.execCommand("copy")) {
          throw new Error("execCommand refused");
        }
        say("Copied", true);
      } catch {
        // The text is selected at this point, so the manual copy is one keystroke; naming the
        // key would be wrong on half the platforms that reach this branch.
        say("Selected — copy it", false);
      }
    });
  }
})();
