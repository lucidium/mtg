/* Liandry Container Registry — app.js
   Progressive enhancement only. No external requests, no routing,
   no path-derived behavior. Site is fully usable without this file. */
(function () {
  "use strict";

  // Close the mobile nav when a link is activated.
  var nav = document.querySelector(".main-nav");
  var toggle = document.getElementById("nav-toggle");
  if (nav && toggle) {
    nav.addEventListener("click", function (e) {
      if (e.target.tagName === "A") {
        toggle.checked = false;
      }
    });
  }

  // Optional "copy" affordance for elements marked [data-copy].
  // Gracefully absent if the Clipboard API is unavailable.
  if (navigator.clipboard && navigator.clipboard.writeText) {
    document.querySelectorAll("[data-copy]").forEach(function (el) {
      var btn = document.createElement("button");
      btn.type = "button";
      btn.className = "copy-btn";
      btn.setAttribute("aria-label", "Copy command to clipboard");
      btn.textContent = "Copy";
      btn.addEventListener("click", function () {
        var text = el.getAttribute("data-copy") || "";
        navigator.clipboard.writeText(text).then(
          function () {
            var prev = btn.textContent;
            btn.textContent = "Copied";
            btn.classList.add("is-copied");
            setTimeout(function () {
              btn.textContent = prev;
              btn.classList.remove("is-copied");
            }, 1400);
          },
          function () { /* silently ignore — clipboard unavailable */ }
        );
      });
      el.appendChild(document.createTextNode(" "));
      el.appendChild(btn);
    });
  }
})();