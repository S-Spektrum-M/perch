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
    await phase('start');
    await waitFor('document.querySelector(".tool-live")?.open === true', 'live output opens automatically');
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
    await phase('506');
    await waitFor('document.querySelector(".tool-live pre")?.textContent.endsWith("line 500\\n")', '500 complete lines streamed');
    await phase('finish');
    await waitFor('!document.querySelector(".tool-live") && document.querySelector(".msg.tool pre")?.textContent.endsWith("line 506\\n")', '506 lines retained on completion');
    assert.equal(await evaluate('document.querySelector(".msg.tool").open'), true);
    assert.equal(await evaluate('document.querySelector(".msg.tool pre").textContent.split("\\n").length - 1'), 506);
    const download = await evaluate('document.querySelector(".msg.tool a[download]").href');
    const completeLog = await (await fetch(download)).text();
    assert.equal(completeLog.split('\n').length - 1, 506);
    await send('Page.reload');
    await waitFor('document.readyState === "complete" && document.querySelector(".msg.tool pre")?.textContent.endsWith("line 506\\n")', 'completed output survives page reload');
    assert.deepEqual(exceptions, [], 'browser JavaScript exceptions');
    console.log('Browser verified: live 100-line chunks, auto-open, SSE reconnect, complete 506-line result, download, and page reload.');
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
