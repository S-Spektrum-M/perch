(() => {
  const filter = document.getElementById("proj-filter");
  if (!filter) return;
  const rows = Array.from(document.querySelectorAll("#project-list .project-row"));
  const empty = document.getElementById("proj-empty");
  filter.addEventListener("input", () => {
    const q = filter.value.trim().toLowerCase();
    let shown = 0;
    for (const row of rows) {
      const match = !q || row.dataset.search.toLowerCase().includes(q);
      row.hidden = !match;
      if (match) shown++;
    }
    if (empty) empty.hidden = shown > 0;
  });
})();
