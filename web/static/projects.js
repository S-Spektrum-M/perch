// Project/session tree on the home page and in the session page's sidebar:
// client-side filtering, current-session highlighting, and the sidebar drawer.
(() => {
  const toggleBtn = document.getElementById("toggle-filter");
  const searchForm = document.getElementById("search-form");
  const filterInput = document.getElementById("home-filter");
  const groups = Array.from(document.querySelectorAll("#project-tree .proj-group"));
  const globalMore = document.getElementById("global-more");
  const treeEmpty = document.getElementById("tree-empty");
  const sidebar = document.getElementById("sidebar");

  // On the session page, mark the open session and reveal it if it's folded away.
  const current = location.pathname.startsWith("/s/") &&
    document.querySelector(`#project-tree .conv-link[href="${CSS.escape(location.pathname)}"]`);
  if (current) {
    current.classList.add("current");
    current.setAttribute("aria-current", "page");
    for (let d = current.closest("details"); d; d = d.parentElement.closest("details")) d.open = true;
  }
  const holdsCurrent = (el) => !!current && el.contains(current);

  if (toggleBtn && searchForm && filterInput) {
    toggleBtn.addEventListener("click", () => {
      searchForm.hidden = !searchForm.hidden;
      if (!searchForm.hidden) {
        filterInput.focus();
      } else if (filterInput.value) {
        filterInput.value = "";
        applyFilter("");
      }
    });

    filterInput.addEventListener("input", () => applyFilter(filterInput.value));
    // The sidebar filters live; submitting would leave the session for the home page,
    // so Enter opens the first match instead.
    if (sidebar) searchForm.addEventListener("submit", (e) => {
      e.preventDefault();
      if (!filterInput.value.trim()) return;
      const first = Array.from(sidebar.querySelectorAll(".conv-link"))
        .find(a => a.getClientRects().length > 0);
      if (first) location.href = first.href;
    });
  }

  function applyFilter(raw) {
    const q = raw.trim().toLowerCase();
    let visibleGroups = 0;
    for (const g of groups) {
      const projMatch = !q || g.dataset.proj.toLowerCase().includes(q);
      const items = Array.from(g.querySelectorAll(".conv-item"));
      const more = g.querySelector("details.proj-more");
      if (projMatch) {
        g.hidden = false;
        for (const it of items) it.hidden = false;
        if (more && !q) more.open = holdsCurrent(more);
        visibleGroups++;
      } else {
        let anyItem = false;
        for (const it of items) {
          const m = it.dataset.title.toLowerCase().includes(q);
          it.hidden = !m;
          if (m) anyItem = true;
        }
        if (more && anyItem) more.open = true;
        g.hidden = !anyItem;
        if (anyItem) visibleGroups++;
      }
    }
    if (globalMore) {
      if (q) {
        const anyMoreVisible = Array.from(globalMore.querySelectorAll(".proj-group")).some(g => !g.hidden);
        globalMore.hidden = !anyMoreVisible;
        if (anyMoreVisible) globalMore.open = true;
      } else {
        globalMore.hidden = false;
        globalMore.open = holdsCurrent(globalMore);
      }
    }
    if (treeEmpty) treeEmpty.hidden = visibleGroups > 0;
  }

  // Narrow screens: the sidebar is a drawer opened from the header.
  const sidebarToggle = document.getElementById("sidebar-toggle");
  const backdrop = document.getElementById("sidebar-backdrop");
  if (sidebar && sidebarToggle) {
    const setOpen = (open) => {
      document.body.classList.toggle("sidebar-open", open);
      sidebarToggle.setAttribute("aria-expanded", String(open));
    };
    sidebarToggle.addEventListener("click", () =>
      setOpen(!document.body.classList.contains("sidebar-open")));
    backdrop?.addEventListener("click", () => setOpen(false));
    document.addEventListener("keydown", (e) => {
      if (e.key === "Escape" && document.body.classList.contains("sidebar-open")) setOpen(false);
    });
  }
})();
