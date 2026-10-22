// Invoked by TestToolStreamBrowser against an isolated Go fixture server.
const assert = require('node:assert/strict');
const {spawn} = require('node:child_process');
const fs = require('node:fs/promises');
const os = require('node:os');
const path = require('node:path');

(async () => {
  const [chrome, base] = process.argv.slice(2);
  const profile = await fs.mkdtemp(path.join(os.tmpdir(), 'pircd-chrome-'));
  const browser = spawn(chrome, [
    '--headless=new', '--no-sandbox', '--disable-dev-shm-usage',
    '--disable-gpu', '--no-first-run', '--disable-background-networking',
    '--remote-debugging-port=0', `--user-data-dir=${profile}`, 'about:blank',
  ], {stdio: 'ignore'});
  let socket;
  try {
    let port;
    for (let i = 0; i < 200; i++) {
      try { port = (await fs.readFile(path.join(profile, 'DevToolsActivePort'), 'utf8')).split('\n')[0]; break; }
      catch { await new Promise(resolve => setTimeout(resolve, 50)); }
    }
    assert.ok(port, 'Chrome did not start');
    const targets = await (await fetch(`http://127.0.0.1:${port}/json/list`)).json();
    socket = new WebSocket(targets.find(target => target.type === 'page').webSocketDebuggerUrl);
    await new Promise((resolve, reject) => { socket.onopen = resolve; socket.onerror = reject; });
    let nextID = 0;
    const pending = new Map();
    const exceptions = [];
    socket.onmessage = ({data}) => {
      const message = JSON.parse(data);
      if (message.method === 'Runtime.exceptionThrown') exceptions.push(message.params.exceptionDetails);
      const waiter = pending.get(message.id);
      if (!waiter) return;
      pending.delete(message.id);
      message.error ? waiter.reject(message.error) : waiter.resolve(message.result);
    };
    const send = (method, params = {}) => new Promise((resolve, reject) => {
      const id = ++nextID;
      pending.set(id, {resolve, reject});
      socket.send(JSON.stringify({id, method, params}));
    });
    const evaluate = async expression => {
      const result = await send('Runtime.evaluate', {expression, returnByValue: true});
      if (result.exceptionDetails) throw new Error(JSON.stringify(result.exceptionDetails));
      return result.result.value;
    };
    const waitFor = async (expression, label) => {
      for (let i = 0; i < 100; i++) {
        if (await evaluate(expression)) return;
        await new Promise(resolve => setTimeout(resolve, 50));
      }
      throw new Error(`Timed out: ${label}`);
    };
    const phase = async name => {
      const res = await fetch(`${base}/fixture/${name}`);
      assert.equal(res.status, 200);
    };
    await send('Runtime.enable');
    await send('Page.enable');
    await send('Page.navigate', {url: `${base}/s/browser`});
    await waitFor('document.querySelector("#transcript") && document.readyState === "complete"', 'initial page');
    const trigger = 'document.querySelector("[data-model-picker]")';
    const card = 'document.querySelector("#model-picker")';
    const openPicker = async () => {
      await evaluate(`${trigger}.click()`);
      await waitFor(`${card}.matches(":popover-open") && !${card}.hasAttribute("aria-busy") && ${card}.querySelector(".model-search")`, 'picker loaded');
      await waitFor(`${trigger}.getAttribute("aria-expanded") === "true"`, 'trigger expanded');
    };
    const escape = async () => {
      await send('Input.dispatchKeyEvent', {type: 'keyDown', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27, nativeVirtualKeyCode: 27});
      await send('Input.dispatchKeyEvent', {type: 'keyUp', key: 'Escape', code: 'Escape', windowsVirtualKeyCode: 27, nativeVirtualKeyCode: 27});
      await waitFor(`!${card}.matches(":popover-open") && ${trigger}.getAttribute("aria-expanded") === "false"`, 'picker dismissed');
    };
    const assertCardPosition = async () => {
      assert.equal(await evaluate(`(() => {
        const rect = ${card}.getBoundingClientRect();
        const anchor = ${trigger}.getBoundingClientRect();
        return rect.left >= 11 && rect.right <= innerWidth - 11 && rect.top >= 11 && rect.bottom <= Math.min(anchor.top, document.querySelector('#promptform').getBoundingClientRect().top) - 7;
      })()`), true, 'card stays above the pill and inside the viewport');
      assert.equal(await evaluate('!!document.querySelector(":modal")'), false, 'model picker is non-modal');
    };
    assert.equal(await evaluate('document.querySelectorAll("#promptform [data-model-picker]").length'), 1);
    assert.equal(await evaluate('document.querySelectorAll("#status [data-model-picker]").length'), 0);
    await openPicker();
    await assertCardPosition();
    assert.equal(await evaluate('document.activeElement.matches(".model-search")'), true);
    await escape();
    assert.equal(await evaluate(`document.activeElement === ${trigger}`), true, 'Escape returns focus to the pill');

    // A late network response must not reopen a dismissed card or steal focus.
    await evaluate(`${trigger}.click()`);
    await escape();
    await new Promise(resolve => setTimeout(resolve, 300));
    assert.equal(await evaluate(`${card}.matches(":popover-open")`), false);
    assert.equal(await evaluate(`document.activeElement === ${trigger}`), true);

    await openPicker();
    await evaluate(`(() => { const input = ${card}.querySelector('.model-search'); input.value = 'other'; input.dispatchEvent(new Event('input', {bubbles:true})); })()`);
    assert.equal(await evaluate(`${card}.querySelectorAll('.model-opt:not([hidden])').length`), 1);
    await send('Input.dispatchKeyEvent', {type: 'keyDown', key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13});
    await send('Input.dispatchKeyEvent', {type: 'keyUp', key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13, nativeVirtualKeyCode: 13});
    await waitFor(`${trigger}.textContent.includes('Other Model') && !${card}.hasAttribute('aria-busy')`, 'selection updates inline pill via SSE');
    assert.equal(await evaluate(`${card}.matches(':popover-open')`), true, 'selection leaves effort controls available');
    await evaluate(`(() => { const slider = ${card}.querySelector('[data-effort-slider]'); slider.value = '1'; slider.dispatchEvent(new Event('change', {bubbles:true})); })()`);
    await waitFor(`${trigger}.textContent.includes('· low') && !${card}.hasAttribute('aria-busy')`, 'effort updates inline pill via SSE');

    for (const width of [375, 320]) {
      await send('Emulation.setDeviceMetricsOverride', {width, height: 740, deviceScaleFactor: 1, mobile: false});
      await new Promise(resolve => setTimeout(resolve, 100));
      await assertCardPosition();
      assert.equal(await evaluate('document.documentElement.scrollWidth <= innerWidth'), true, 'no mobile horizontal overflow');
      assert.equal(await evaluate('document.querySelectorAll("#mobile-model [data-model-picker]").length'), 1, 'mobile selector moves alongside stats');
      assert.equal(await evaluate('document.querySelectorAll("#promptform [data-model-picker]").length'), 0, 'mobile prompt has no model selector');
      assert.ok(await evaluate('document.querySelector("#message").getBoundingClientRect().width') > width - 150, 'mobile typing area reclaims model selector width');
    }
    await evaluate(`${card}.querySelector('[data-picker-close]').click()`);
    await waitFor(`!${card}.matches(':popover-open')`, 'Done closes card');
    await evaluate('document.querySelector("#message").value = "First line\\nSecond line\\nThird line"; document.querySelector("#message").dispatchEvent(new Event("input"))');
    assert.equal(await evaluate(`${trigger}.getBoundingClientRect().bottom <= document.querySelector('#promptform').getBoundingClientRect().top`), true, 'mobile model selector stays above multiline prompts');
    await openPicker();
    await assertCardPosition();
    // Native light dismiss: clicking the textarea closes the card without blocking typing.
    const point = await evaluate('(() => { const r = document.querySelector("#message").getBoundingClientRect(); return {x:r.left+r.width/2,y:r.top+r.height/2}; })()');
    await send('Input.dispatchMouseEvent', {type: 'mousePressed', button: 'left', clickCount: 1, ...point});
    await send('Input.dispatchMouseEvent', {type: 'mouseReleased', button: 'left', clickCount: 1, ...point});
    await waitFor(`!${card}.matches(':popover-open')`, 'outside click dismisses card');
    assert.equal(await evaluate('document.activeElement.id'), 'message');
    await evaluate('document.querySelector("#message").value = ""; document.querySelector("#message").dispatchEvent(new Event("input"))');
    await send('Emulation.clearDeviceMetricsOverride');
    await waitFor('document.querySelectorAll("#promptform [data-model-picker]").length === 1', 'desktop selector restored after resize');

    await phase('models-curated');
    await openPicker();
    assert.deepEqual(await evaluate(`Array.from(${card}.querySelectorAll('.model-opt'), opt => opt.dataset.modelId)`), ['missing', 'other', 'test'], 'file controls membership and order');
    await waitFor(`${trigger}.textContent.includes('Favorite')`, 'friendly label reaches prompt');
    assert.equal(await evaluate(`${card}.querySelector('[data-model-id="missing"]').disabled`), true);
    await evaluate(`(() => { const slider = ${card}.querySelector('[data-effort-slider]'); slider.value = '2'; slider.dispatchEvent(new Event('change', {bubbles:true})); })()`);
    await waitFor(`!${card}.hasAttribute('aria-busy') && ${trigger}.textContent.includes('· high')`, 'effort save with unavailable entries');
    assert.equal(await evaluate(`${card}.querySelector('[data-model-id="missing"]').disabled`), true, 'save must not re-enable unavailable entries');
    await evaluate(`(() => { const input = ${card}.querySelector('.model-search'); input.value = 'missing'; input.dispatchEvent(new Event('input', {bubbles:true})); input.focus(); })()`);
    await send('Input.dispatchKeyEvent', {type: 'keyDown', key: 'Enter', code: 'Enter', windowsVirtualKeyCode: 13});
    assert.equal(await evaluate(`${card}.querySelectorAll('.model-opt.active').length`), 0, 'keyboard navigation skips unavailable entries');
    assert.equal(await evaluate(`${card}.hasAttribute('aria-busy')`), false, 'unavailable entry cannot be selected with Enter');
    await escape();

    await phase('models-edited');
    await openPicker();
    assert.deepEqual(await evaluate(`Array.from(${card}.querySelectorAll('.model-opt'), opt => opt.dataset.modelId)`), ['test'], 'file edits reload without restart');
    await waitFor(`${trigger}.textContent.includes('Other Model')`, 'unlisted current model keeps its actual name');
    await escape();

    await phase('models-invalid');
    await evaluate(`${trigger}.click()`);
    await waitFor(`${card}.querySelector('[data-picker-error]')?.textContent.includes('invalid models config') && !${card}.hasAttribute('aria-busy')`, 'invalid config shown inside card');
    assert.equal(await evaluate(`${card}.querySelectorAll('.model-opt').length`), 0, 'invalid reload clears stale choices rather than falling back');
    await phase('models-edited');
    await evaluate(`${card}.querySelector('[data-picker-retry]').click()`);
    await waitFor(`${card}.querySelector('.model-search') && !${card}.hasAttribute('aria-busy')`, 'retry recovers after fixing config');
    await escape();

    await phase('models-empty');
    await openPicker();
    assert.equal(await evaluate(`${card}.querySelectorAll('.model-opt').length`), 0, 'empty config does not fall back to pi');
    assert.equal(await evaluate(`${card}.querySelector('.model-empty').hidden`), false, 'empty config explains the empty list');
    await escape();
    await phase('models-remove');
    await openPicker();
    assert.deepEqual(await evaluate(`Array.from(${card}.querySelectorAll('.model-opt'), opt => opt.dataset.modelId)`), ['test', 'other'], 'missing file restores pi catalog');
    await escape();

    const promptState = async (state, variable, label) => {
      const hex = await evaluate(`getComputedStyle(document.documentElement).getPropertyValue(${JSON.stringify(variable)}).trim()`);
      const color = `rgb(${hex.slice(1).match(/../g).map(part => parseInt(part, 16)).join(', ')})`;
      await waitFor(`document.querySelector('[data-prompt-state]')?.dataset.promptState === ${JSON.stringify(state)} && getComputedStyle(document.querySelector('#promptform')).borderTopColor === ${JSON.stringify(color)}`, `prompt border: ${state}`);
      assert.equal(await evaluate('document.querySelector("#promptform").title'), label);
      assert.equal(await evaluate('document.querySelector("#prompt-status").textContent'), label);
      await evaluate('document.querySelector("#message").focus()');
      assert.equal(await evaluate('getComputedStyle(document.querySelector("#promptform")).borderTopColor'), color, 'focus preserves the state color');
      assert.equal(await evaluate('document.querySelectorAll(".statusline .idle, .statusline .activity, .statusline .spinner").length'), 0);
    };
    for (const theme of ['light', 'dark']) {
      await send('Emulation.setEmulatedMedia', {features: [{name: 'prefers-color-scheme', value: theme}]});
      await promptState('idle', '--muted', 'idle');
      await phase('working');
      await promptState('busy', '--warn', 'working');
      await phase('responding');
      await promptState('responding', '--accent', 'responding');
      await send('Emulation.setDeviceMetricsOverride', {width: 320, height: 740, deviceScaleFactor: 1, mobile: false});
      await waitFor('innerWidth === 320', 'narrow streaming layout');
      assert.ok(await evaluate('document.querySelector("#message").getBoundingClientRect().width') >= 60, 'streaming controls leave typing room on mobile');
      assert.equal(await evaluate('document.documentElement.scrollWidth <= innerWidth'), true, 'streaming layout has no horizontal overflow');
      await send('Emulation.clearDeviceMetricsOverride');
      await send('Page.reload');
      await waitFor('document.readyState === "complete" && document.querySelector("#message")', 'responding page reload');
      await promptState('responding', '--accent', 'responding');
      await phase('idle');
      await promptState('idle', '--muted', 'idle');
      await phase('compacting');
      await promptState('busy', '--warn', 'compacting context');
      await phase('compacted');
      await promptState('idle', '--muted', 'idle');
    }
    await phase('start');
    await waitFor('document.querySelector(".tool-live")?.open === false', 'live output starts folded');
    await evaluate('document.querySelector(".tool-live summary").click()');
    await phase('99');
    await new Promise(resolve => setTimeout(resolve, 150));
    assert.equal(await evaluate('document.querySelector(".tool-live pre").textContent'), '');
    await phase('200');
    await waitFor('document.querySelector(".tool-live pre")?.textContent.endsWith("line 200\\n")', '200 lines arrive before completion');
    assert.equal(await evaluate('document.querySelectorAll(".tool-live pre")[0].textContent.split("\\n").length - 1'), 200);
    await waitFor('(() => { const pre = document.querySelector(".tool-live pre"); return pre.scrollHeight - pre.scrollTop - pre.clientHeight < 40; })()', 'live output follows the newest lines');
    await evaluate('document.querySelector(".tool-live pre").scrollTop = 0');
    await phase('300');
    await waitFor('document.querySelector(".tool-live pre")?.textContent.endsWith("line 300\\n")', 'next chunk while reading earlier output');
    assert.equal(await evaluate('document.querySelector(".tool-live pre").scrollTop'), 0, 'do not interrupt user scrolling');
    await send('Page.reload');
    await waitFor('document.readyState === "complete" && document.querySelector(".tool-live pre")?.textContent.endsWith("line 300\\n")', 'live reload and SSE reconnect');
    assert.equal(await evaluate('document.querySelector(".tool-live").open'), false, 'live output is folded after reload');
    await evaluate('document.querySelector(".tool-live summary").click()');
    await phase('506');
    await waitFor('document.querySelector(".tool-live pre")?.textContent.endsWith("line 500\\n")', '500 complete lines streamed');
    await phase('finish');
    await waitFor('!document.querySelector(".tool-live") && document.querySelector(".msg.tool pre")?.textContent.endsWith("line 506\\n")', '506 lines retained on completion');
    assert.equal(await evaluate('document.querySelector(".msg.tool").open'), true, 'manual expansion survives completion');
    assert.equal(await evaluate('document.querySelector(".msg.tool pre").textContent.split("\\n").length - 1'), 506);
    const download = await evaluate('document.querySelector(".msg.tool a[download]").href');
    const completeLog = await (await fetch(download)).text();
    assert.equal(completeLog.split('\n').length - 1, 506);
    await send('Page.reload');
    await waitFor('document.readyState === "complete" && document.querySelector(".msg.tool pre")?.textContent.endsWith("line 506\\n")', 'completed output survives page reload');
    await phase('exit');
    await promptState('exited', '--danger', 'pi exited (test)');
    assert.equal(await evaluate('document.querySelector(".statusline .exited").textContent'), 'pi exited (test)');
    assert.equal(await evaluate(`${trigger}.disabled`), true, 'exited session disables the inline picker');
    assert.deepEqual(exceptions, [], 'browser JavaScript exceptions');
    console.log('Browser verified: config-driven model order/labels, availability, reload and config-error recovery; inline model/effort picker, anchored non-modal card, mobile layout, loading dismissal, Escape and outside click; prompt status colors, focus, light/dark themes, live updates and reload; live 100-line chunks, folded defaults, manual expansion, SSE reconnect, complete 506-line result, and download.');
  } finally {
    socket?.close();
    if (browser.exitCode === null && browser.signalCode === null) {
      const exited = new Promise(resolve => browser.once('exit', resolve));
      browser.kill('SIGTERM');
      await exited;
    }
    await fs.rm(profile, {recursive: true, force: true, maxRetries: 5, retryDelay: 100});
  }
})().catch(error => { console.error(error); process.exitCode = 1; });
