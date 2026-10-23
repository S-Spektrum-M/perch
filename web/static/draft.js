// Draft page: the prompt box of a session that starts with its first message.
// The form posts normally; this adds Enter to send, autosizing, an attachment
// count, the model list for the picked agent, and remembering both picks.
(() => {
  const form = document.getElementById("promptform");
  const message = document.getElementById("message");
  const files = document.getElementById("file-input");
  const choice = form.querySelector(".engine-choice");
  const model = document.getElementById("draft-model");
  const key = "pircd.engine";
  const modelKey = (e) => `pircd.model.${e}`;

  // Preselect the last agent used, unless the link asked for one.
  if (choice && !choice.dataset.preset) {
    let last = null;
    try { last = localStorage.getItem(key); } catch {}
    const radio = last && choice.querySelector(`input[value="${CSS.escape(last)}"]`);
    if (radio) radio.checked = true;
  }
  const engine = () => form.querySelector('input[name="engine"]:checked, input[name="engine"][type="hidden"]')?.value || "";

  const touch = window.matchMedia("(pointer: coarse)");
  const setPlaceholder = () => {
    const to = `Message ${engine() || "the agent"}`;
    message.placeholder = touch.matches ? to : `${to} (Enter to send, Shift+Enter for newline)`;
  };
  setPlaceholder();
  touch.addEventListener("change", setPlaceholder);
  // The model list depends on the engine: load it from the server, then
  // restore the model given back after a failed start, else the last used.
  let wanted = model.dataset.selected;
  let loading = 0;
  const loadModels = async () => {
    const e = engine();
    const n = ++loading;
    model.disabled = true;
    model.title = "Loading models…";
    try {
      const res = await fetch(`/draft/models?engine=${encodeURIComponent(e)}`);
      if (!res.ok) throw new Error(await res.text());
      const html = await res.text();
      if (n !== loading) return; // the engine changed meanwhile
      model.innerHTML = html;
      let pick = wanted;
      try { pick ||= localStorage.getItem(modelKey(e)); } catch {}
      wanted = "";
      if (pick && [...model.options].some((o) => o.value === pick)) model.value = pick;
      model.title = "Model";
    } catch (err) {
      if (n !== loading) return;
      model.innerHTML = '<option value="">Default model</option>';
      model.title = `Could not list models: ${err.message}`;
    }
    model.disabled = false;
  };
  loadModels();
  model.addEventListener("change", () => {
    try { localStorage.setItem(modelKey(engine()), model.value); } catch {}
  });

  choice?.addEventListener("change", () => {
    try { localStorage.setItem(key, engine()); } catch {}
    setPlaceholder();
    loadModels();
    message.focus();
  });

  const autosize = () => {
    message.style.height = "auto";
    message.style.height = Math.min(message.scrollHeight, window.innerHeight * 0.4) + "px";
  };
  message.addEventListener("input", autosize);
  autosize();

  const label = files.closest(".attach-btn");
  files.addEventListener("change", () => {
    const n = files.files.length;
    label.title = n ? `${n} file${n === 1 ? "" : "s"} attached` : "Attach files";
    label.classList.toggle("has-files", n > 0);
  });

  let sending = false;
  form.addEventListener("submit", (e) => {
    if (sending || (!message.value.trim() && !files.files.length)) {
      e.preventDefault();
      return;
    }
    sending = true;
    try { localStorage.setItem(key, engine()); } catch {}
    form.querySelector("#send").disabled = true;
  });
  message.addEventListener("keydown", (e) => {
    if (e.key === "Enter" && !e.shiftKey && !e.isComposing && !touch.matches) {
      e.preventDefault();
      form.requestSubmit();
    }
  });
})();
