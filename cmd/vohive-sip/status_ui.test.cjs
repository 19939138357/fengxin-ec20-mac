const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { chromium } = require('playwright');

// Isolated browser fixture: no real gateway, serial port, or messages touched.
(async () => {
  const browser = await chromium.launch({ headless: true, executablePath: process.env.VOHIVE_TEST_BROWSER || undefined });
  try {
    const page = await browser.newPage();
    await page.route('http://vohive-status.test/**', async route => {
      const target = new URL(route.request().url()).pathname;
      let data;
      if (target === '/') return route.fulfill({ contentType: 'text/html', body: fs.readFileSync(path.join(__dirname, 'index.html'), 'utf8') });
      if (target === '/api/status') data = {
        version: 'test', sip_listen: '127.0.0.1:5068', sip_username: 'test', sip_registered: 1, audio_device: 'fixture',
        modem: { connected: false, registered: true, last_updated: '2026-09-24T00:00:00Z', port: 'COM14', model: 'EC20',
          sim: 'OLD_SIM_VALUE', registration: 'OLD_REG_VALUE', signal: 'OLD_SIGNAL_VALUE', radio: 'OLD_RADIO_VALUE', operator: 'OLD_OPERATOR_VALUE', error: 'Serial port closed; restart gateway' }
      };
      else if (target === '/api/inbox') data = { messages: [], receiver: { running: false, ready: false, error: '' } };
      else if (target === '/api/notifications/events') data = [];
      else if (target === '/api/notifications/settings') data = { enabled: false };
      else return route.fulfill({ status: 404 });
      return route.fulfill({ contentType: 'application/json', body: JSON.stringify(data) });
    });
    await page.goto('http://vohive-status.test/');
    await page.waitForFunction(() => document.getElementById('version').textContent.includes('test'));
    const status = await page.locator('#state').textContent();
    assert.match(status, /串口已断开/, 'serial disconnection must be distinct from no cellular registration');
    assert.doesNotMatch(await page.locator('#modem').textContent(), /OLD_/, 'disconnected values must not appear as current readings');
    assert.match(await page.locator('#modem').textContent(), /状态更新时间/, 'show when connection status was obtained');
    assert.match(await page.locator('#receiver').textContent(), /短信接收已停止/, 'stopped receiver must not look like it is still initializing');
    assert.equal(await page.locator('#send').isDisabled(), true, 'cannot submit an SMS while the serial connection is closed');
    console.log('PASS: disconnected status hides old readings and exposes stopped reception');
  } finally { await browser.close(); }
})().catch(error => { console.error(error.message); process.exitCode = 1; });
