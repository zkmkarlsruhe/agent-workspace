// Progressive enhancement only: every flow works without this file.
// No inline handlers, no eval (CSP script-src 'self').
(function () {
  "use strict";

  // .js lets CSS hide the native file input behind its German label button.
  document.documentElement.classList.add("js");
  var EN = document.documentElement.lang === "en";

  // requestSubmit preserves native validation and the existing submit handlers.
  document.addEventListener("keydown", function (ev) {
    var ta = ev.target;
    if (ev.key !== "Enter" || !(ev.ctrlKey || ev.metaKey) || ev.isComposing || ev.repeat ||
        !ta.matches("textarea[data-submit-shortcut]") || !ta.form || ta.disabled) return;
    ev.preventDefault();
    if (ta.form.getAttribute("aria-busy") !== "true") ta.form.requestSubmit();
  });

  // Confirmation links (a[data-dialog]) point at a confirmation page. With JS
  // we fetch that page and show its <dialog> modally instead of navigating.
  document.addEventListener("click", function (ev) {
    var close = ev.target.closest("dialog [data-dialog-close]");
    if (close && close.closest("dialog").matches(":modal")) {
      ev.preventDefault();
      close.closest("dialog").close();
      return;
    }
    var link = ev.target.closest("a[data-dialog]");
    if (!link || ev.ctrlKey || ev.metaKey || ev.shiftKey) return;
    ev.preventDefault();
    fetch(link.href, { credentials: "same-origin" })
      .then(function (r) { return r.ok ? r.text() : Promise.reject(r.status); })
      .then(function (html) {
        var doc = new DOMParser().parseFromString(html, "text/html");
        var dlg = doc.querySelector("dialog.dialog");
        if (!dlg) throw new Error("no dialog");
        dlg = document.importNode(dlg, true);
        dlg.removeAttribute("open");
        dlg.classList.remove("dialog--inline");
        dlg.addEventListener("close", function () { dlg.remove(); link.focus(); });
        document.body.appendChild(dlg);
        dlg.showModal(); // focuses the [autofocus] "Abbrechen"
      })
      .catch(function () { window.location.href = link.href; });
  });

  // Upload progress for the htmx upload form.
  document.addEventListener("htmx:xhr:progress", function (ev) {
    var form = ev.target.closest("form");
    var box = form && form.querySelector(".upload-progress");
    if (!box || !ev.detail.lengthComputable) return;
    var pct = Math.round((ev.detail.loaded / ev.detail.total) * 100);
    box.hidden = false;
    box.querySelector("progress").value = pct;
    box.querySelector("progress").textContent = pct + " %";
    box.querySelector(".upload-progress__row span:last-child").textContent = pct + " %";
  });

  // Server errors (5xx) are full German error pages: show them as the page.
  document.addEventListener("htmx:responseError", function (ev) {
    document.open();
    document.write(ev.detail.xhr.responseText);
    document.close();
  });

  // File picker: name the chosen files next to the button.
  document.addEventListener("change", function (ev) {
    var input = ev.target;
    var list = input.matches && input.matches(".filepick__input") && input.parentNode.querySelector(".filepick__list");
    if (!list) return;
    var names = Array.prototype.map.call(input.files, function (f) { return f.name; });
    var n = names.length;
    var text = names.slice(0, 5).join(", ") + (n > 5 ? (EN ? " and " + (n - 5) + " more" : " und " + (n - 5) + " weitere") : "");
    list.textContent = n === 0 ? "" : (n === 1 ? (EN ? "1 file: " : "1 Datei: ") : n + (EN ? " files: " : " Dateien: ")) + text;
  });

  // Start form: refuse an unfilled template gap like [Thema] before the files
  // are sent (the server checks the same, but the file selection would be lost).
  var PLACEHOLDER = /\[[^\]\n]*\]/;
  document.addEventListener("submit", function (ev) {
    var form = ev.target;
    var ta = form.matches("form[data-brief]") && form.querySelector("textarea[name=description]");
    if (!ta) return;
    if (!PLACEHOLDER.test(ta.value)) {
      once(ev);
      return;
    }
    ev.preventDefault();
    var err = document.getElementById(ta.id + "-error");
    err.textContent = EN ? "First replace the parts in square brackets, for example [topic], with your details." : "Ersetze zuerst die Stellen in eckigen Klammern, zum Beispiel [Thema], durch deine Angaben.";
    err.hidden = false;
    ta.setAttribute("aria-invalid", "true");
    var ids = (ta.getAttribute("aria-describedby") || "").split(" ");
    if (ids.indexOf(err.id) < 0) ta.setAttribute("aria-describedby", ids.concat(err.id).join(" ").trim());
    ta.closest(".field").classList.add("field--invalid");
    ta.focus();
  });

  // Clear the placeholder message once the gaps are filled in.
  document.addEventListener("input", function (ev) {
    var ta = ev.target;
    if (!ta.matches("form[data-brief] textarea[name=description]") || PLACEHOLDER.test(ta.value)) return;
    var err = document.getElementById(ta.id + "-error");
    if (!err || err.hidden) return;
    err.hidden = true;
    ta.removeAttribute("aria-invalid");
    ta.closest(".field").classList.remove("field--invalid");
  });

  // Forms that start a paid run (start form, data-once) are sent once: a
  // double click must not create two projects or chats. The button is
  // disabled after the browser has collected the form data (a submitter
  // disabled inside the submit event would drop its name/value).
  function once(ev) {
    var form = ev.target;
    if (form.getAttribute("aria-busy") === "true") {
      ev.preventDefault();
      return;
    }
    form.setAttribute("aria-busy", "true");
    var btn = form.querySelector("button[type=submit]");
    if (!btn) return;
    btn.dataset.label = btn.innerHTML;
    setTimeout(function () {
      btn.disabled = true;
      btn.textContent = EN ? "Creating ..." : "Wird angelegt ...";
    }, 0);
  }
  document.addEventListener("submit", function (ev) {
    if (ev.target.matches("form[data-once]")) once(ev);
  });
  // Back/forward cache: a restored page gets its buttons back.
  window.addEventListener("pageshow", function (ev) {
    if (!ev.persisted) return;
    document.querySelectorAll("form[aria-busy=true]").forEach(function (form) {
      form.removeAttribute("aria-busy");
      var btn = form.querySelector("button[type=submit]");
      if (!btn) return;
      btn.disabled = false;
      if (btn.dataset.label) btn.innerHTML = btn.dataset.label;
    });
  });

  // Drag-over styling for the drop zone; dropping fills the file input.
  var ZONE = ".dropzone, [data-drop]";
  document.addEventListener("dragover", function (ev) {
    var zone = ev.target.closest && ev.target.closest(ZONE);
    if (!zone) return;
    ev.preventDefault();
    zone.classList.add("is-dragover");
  });
  document.addEventListener("dragleave", function (ev) {
    var zone = ev.target.closest && ev.target.closest(ZONE);
    if (zone && !zone.contains(ev.relatedTarget)) zone.classList.remove("is-dragover");
  });
  document.addEventListener("drop", function (ev) {
    var zone = ev.target.closest && ev.target.closest(ZONE);
    if (!zone) return;
    ev.preventDefault();
    zone.classList.remove("is-dragover");
    var input = zone.querySelector("input[type=file]");
    if (input && ev.dataTransfer.files.length) {
      input.files = ev.dataTransfer.files;
      input.dispatchEvent(new Event("change", { bubbles: true }));
    }
  });
})();

// Project page: upload as soon as files are chosen or dropped. requestSubmit
// fires the submit event, so htmx's hx-post with the progress bar handles it.
(function () {
  "use strict";
  document.addEventListener("change", function (ev) {
    var input = ev.target;
    if (!input.matches || !input.matches("input[type=file]") || !input.files.length) return;
    var form = input.closest("form[data-autoupload]");
    if (form) form.requestSubmit();
  });
})();
