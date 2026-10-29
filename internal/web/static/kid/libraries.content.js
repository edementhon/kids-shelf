// Kids Shelf guard for library pages (kiwix-serve /kiwix/content/...,
// libraries origin). The libraries proxy adds this script to every book
// page. It does two things:
//  - a book page opened on its own (a link opened in a new tab, say) moves
//    into the reader, so the Kids Shelf bar with its big Home button is
//    always there;
//  - every link and form that leads outside the library is pointed at the
//    "not in the library" page instead, so no tap, long-press, middle-click
//    or new tab can reach the open internet.
'use strict';
(function () {
  var BLOCKED = '/kiwix/catch/external';

  if (window.top === window) {
    var m = location.pathname.match(/^\/kiwix\/content\/(.+)$/);
    if (m) {
      location.replace('/kiwix/viewer#' + m[1] + location.hash);
      return;
    }
  }

  // outside reports whether a link target leaves this library: another
  // origin, or a scheme an outside app would handle (mailto:, tel:, ...).
  function outside(raw) {
    if (raw == null) return false;
    var u;
    try { u = new URL(raw, location.href); } catch (e) { return false; }
    if (u.protocol === 'javascript:' || u.protocol === 'about:' ||
        u.protocol === 'data:' || u.protocol === 'blob:') return false;
    return u.origin !== location.origin;
  }

  function guard(el) {
    if (el.tagName === 'FORM') {
      if (outside(el.getAttribute('action'))) {
        el.setAttribute('action', BLOCKED);
        el.setAttribute('method', 'get');
        el.removeAttribute('target');
      }
      return;
    }
    if (outside(el.getAttribute('href'))) {
      el.setAttribute('href', BLOCKED);
      el.removeAttribute('target');
    }
  }

  function guardAll(root) {
    var els = root.querySelectorAll('a[href], area[href], form[action]');
    for (var i = 0; i < els.length; i++) guard(els[i]);
  }

  // Links added after load are caught the moment they are used, before the
  // reader's own click handling sees them (capture phase).
  function onUse(e) {
    var t = e.target;
    while (t && t.nodeType === 1) {
      if (t.tagName === 'A' || t.tagName === 'AREA' || t.tagName === 'FORM') {
        guard(t);
        return;
      }
      t = t.parentNode;
    }
  }
  ['click', 'auxclick', 'contextmenu', 'dragstart', 'submit', 'pointerdown'].forEach(function (type) {
    window.addEventListener(type, onUse, true);
  });

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', function () { guardAll(document); });
  } else {
    guardAll(document);
  }
})();
