// Draws build/index.html (see build.py) with headless Chrome ($CHROME):
//
//   node render.js stills 1.2 3.4 …          → build/stills/t-1.20.png
//   node render.js video FROM TO OUT.mp4 SUB  → 60 fps, each frame the mean of SUB subframes:
//                                               the camera blurs, the terminal does not
const puppeteer = require('puppeteer-core');
const { spawn } = require('child_process');
const fs = require('fs');
const path = require('path');
const CHROME = process.env.CHROME || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';

(async () => {
  const [mode, ...rest] = process.argv.slice(2);
  const browser = await puppeteer.launch({ executablePath: CHROME, headless: 'new',
    args: ['--hide-scrollbars', '--force-device-scale-factor=1', '--allow-file-access-from-files', '--font-render-hinting=none'] });
  const page = await browser.newPage();
  await page.setViewport({ width: 1920, height: 1080, deviceScaleFactor: 1 });
  await page.goto('file://' + path.join(__dirname, 'build', 'index.html'));
  // Draw the outro once, so its fonts load before any frame is taken.
  await page.evaluate(() => render(window.DURATION - 1));
  await page.evaluate(async () => { for (const f of ['900 100px Archivo', '700 100px Archivo', '800 40px "JetBrains Mono"', '400 40px "JetBrains Mono"']) await document.fonts.load(f); await document.fonts.ready; await Promise.all([...document.images].map(i => i.decode().catch(() => {}))); });
  const cdp = await page.target().createCDPSession();
  const shot = async (t, tc = t) => {
    await page.evaluate(([t, tc]) => render(t, tc), [t, tc]);
    const { data } = await cdp.send('Page.captureScreenshot', { format: 'png', optimizeForSpeed: true });
    return Buffer.from(data, 'base64');
  };
  if (mode === 'stills') {
    fs.mkdirSync(path.join(__dirname, 'build', 'stills'), { recursive: true });
    for (const s of rest) fs.writeFileSync(path.join(__dirname, 'build', 'stills', `t-${parseFloat(s).toFixed(2)}.png`), await shot(parseFloat(s)));
  } else {
    const from = parseFloat(rest[0]), to = parseFloat(rest[1]), out = rest[2], sub = parseInt(rest[3] || '1');
    const rate = 60 * sub;
    // Each output frame is the mean of its own sub subframes, n = sub·k … sub·k + sub − 1.
    const vf = sub > 1 ? `tmix=frames=${sub},select='eq(mod(n\\,${sub})\\,${sub - 1})',setpts=N/60/TB` : 'fps=60';
    const ff = spawn('ffmpeg', ['-y', '-loglevel', 'error', '-f', 'image2pipe', '-framerate', String(rate), '-c:v', 'png', '-i', '-',
      '-vf', vf, '-c:v', 'libx264', '-preset', 'slow', '-crf', '14', '-pix_fmt', 'yuv420p', '-r', '60', out], { stdio: ['pipe', 'inherit', 'inherit'] });
    const n0 = Math.round(from * rate), n1 = Math.round(to * rate);
    for (let f = n0; f < n1; f++) {
      const buf = await shot(f / rate, Math.floor(f / sub) / 60);
      if (!ff.stdin.write(buf)) await new Promise(r => ff.stdin.once('drain', r));
    }
    ff.stdin.end();
    await new Promise(r => ff.on('close', r));
    console.log('wrote', out);
  }
  browser.close().catch(() => {});
  setTimeout(() => process.exit(0), 500);
})();
