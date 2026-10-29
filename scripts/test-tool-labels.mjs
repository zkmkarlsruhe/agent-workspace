// SPDX-License-Identifier: MIT
// Run with Node and Playwright installed; PLAYWRIGHT_MODULE may select an existing installation.
import assert from 'node:assert/strict'
import fs from 'node:fs/promises'
const { chromium } = await import(process.env.PLAYWRIGHT_MODULE || 'playwright')
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_PATH || '/usr/bin/chromium' })
try {
  const page = await browser.newPage()
  await page.setContent(`<html lang="de"><body>
    <p id="prose">workspace_freecad_open</p><pre id="code">workspace_freecad_open</pre>
    <span data-slot="basic-tool-tool-subtitle">workspace_freecad_open</span>
    <span data-slot="basic-tool-tool-title" id="running"><span aria-label="Called workspace_freecad_open">
      <span aria-hidden="true">Called workspace_freecad_open</span><span aria-hidden="true">Called workspace_freecad_open</span>
    </span></span>
    <span data-slot="basic-tool-tool-title" id="group">workspace_freecad_open, workspace_freecad_save</span>
    <span data-slot="basic-tool-tool-title" id="unknown">other_freecad_open workspace_freecad_open_extra</span>
    <span data-slot="basic-tool-tool-title" id="error">workspace_freecad_save</span>
  </body></html>`)
  await page.evaluate(() => { window.originalToolNode = document.querySelector('#running span span').firstChild })
  const script = await fs.readFile(new URL('../manager/internal/router/brand-tools.js', import.meta.url), 'utf8')
  await page.addScriptTag({ content: `const toolDisplayTitles = ${JSON.stringify({
    de: { workspace_freecad_open: 'FreeCAD öffnen', workspace_freecad_save: 'FreeCAD-Modell speichern' },
    en: { workspace_freecad_open: 'Open FreeCAD', workspace_freecad_save: 'Save FreeCAD model' },
  })};\n${script}` })
  await page.waitForFunction(() => document.querySelector('#group').textContent === 'FreeCAD öffnen, FreeCAD-Modell speichern')
  assert.equal(await page.locator('#running [aria-label]').getAttribute('aria-label'), 'Called FreeCAD öffnen')
  assert.equal(await page.locator('#error').textContent(), 'FreeCAD-Modell speichern')
  assert.equal(await page.locator('#prose').textContent(), 'workspace_freecad_open')
  assert.equal(await page.locator('#code').textContent(), 'workspace_freecad_open')
  assert.equal(await page.locator('[data-slot="basic-tool-tool-subtitle"]').textContent(), 'workspace_freecad_open')
  assert.equal(await page.locator('#unknown').textContent(), 'other_freecad_open workspace_freecad_open_extra')
  assert.equal(await page.evaluate(() => document.querySelector('#running span span').firstChild === window.originalToolNode), true)
  await page.evaluate(() => { document.documentElement.lang = 'en-US' })
  await page.waitForFunction(() => document.querySelector('#group').textContent === 'Open FreeCAD, Save FreeCAD model')
  assert.equal(await page.locator('#running [aria-label]').getAttribute('aria-label'), 'Called Open FreeCAD')
  await page.evaluate(() => {
    window.originalToolNode.nodeValue = 'Called workspace_freecad_save'
    const title = document.createElement('span')
    title.dataset.slot = 'basic-tool-tool-title'
    title.id = 'new'
    title.textContent = 'workspace_freecad_open'
    document.body.append(title)
  })
  await page.waitForFunction(() => document.querySelector('#new').textContent === 'Open FreeCAD' && window.originalToolNode.nodeValue === 'Called Save FreeCAD model')
  await page.evaluate(() => { document.documentElement.lang = 'de-DE' })
  await page.waitForFunction(() => document.querySelector('#new').textContent === 'FreeCAD öffnen' && window.originalToolNode.nodeValue === 'Called FreeCAD-Modell speichern')
  const remote = await browser.newPage()
  const requests = []
  let releaseSlow
  const slow = new Promise(resolve => { releaseSlow = resolve })
  await remote.route('https://workspace.test/**', async route => {
    const url = new URL(route.request().url())
    if (url.pathname !== '/__workspace/tool-titles') {
      return route.fulfill({ contentType: 'text/html', body: '<html lang="de"><body><span id="external" data-slot="basic-tool-tool-title">library_search</span><span id="own" data-slot="basic-tool-tool-title">workspace_freecad_open</span></body></html>' })
    }
    const session = url.searchParams.get('session')
    requests.push(session)
    if (session === 'ses_second0002') await slow
    if (session === 'ses_failed0004') return route.fulfill({ status: 503, body: 'unavailable' })
    const title = session === 'ses_first0001' ? '<img src=x onerror=alert(1)> Books' : session === 'ses_second0002' ? 'Stale title' : 'Fresh title'
    await route.fulfill({ contentType: 'application/json', body: JSON.stringify({ library_search: title, workspace_freecad_open: 'Untrusted override' }) }).catch(() => {})
  })
  await remote.goto('https://workspace.test/session/ses_first0001')
  await remote.addScriptTag({ content: `const toolDisplayTitles = { de: { workspace_freecad_open: 'FreeCAD öffnen' }, en: { workspace_freecad_open: 'Open FreeCAD' } }; const externalToolTitlesURL = '/__workspace/tool-titles';\n${script}` })
  await remote.waitForFunction(() => document.querySelector('#external').textContent === '<img src=x onerror=alert(1)> Books')
  assert.equal(await remote.locator('img').count(), 0)
  assert.equal(await remote.locator('#own').textContent(), 'FreeCAD öffnen')
  const navigate = async session => remote.evaluate(session => {
    history.pushState({}, '', '/session/' + session)
    document.querySelector('#external').firstChild.nodeValue = 'library_search'
  }, session)
  await navigate('ses_second0002')
  await remote.waitForFunction(() => document.querySelector('#external').textContent === 'library_search')
  // Wait until the slow request was issued, then navigate before it can return.
  for (let n = 0; n < 100 && !requests.includes('ses_second0002'); n++) await remote.waitForTimeout(10)
  assert.ok(requests.includes('ses_second0002'))
  await navigate('ses_third0003')
  await remote.waitForFunction(() => document.querySelector('#external').textContent === 'Fresh title')
  releaseSlow()
  await remote.waitForTimeout(100)
  assert.equal(await remote.locator('#external').textContent(), 'Fresh title')
  await remote.evaluate(() => { document.documentElement.lang = 'en-US' })
  await remote.waitForFunction(() => document.querySelector('#own').textContent === 'Open FreeCAD')
  assert.equal(await remote.locator('#external').textContent(), 'Fresh title')
  await navigate('ses_failed0004')
  await remote.waitForTimeout(100)
  assert.equal(await remote.locator('#external').textContent(), 'library_search')
  assert.equal(requests.length, 4, 'DOM and language updates must not repeatedly discover tools')
  console.log('PASS: tool titles, groups, errors, streaming, accessible names, languages; external discovery, session isolation, stale responses, failures and plain text rendering')
} finally { await browser.close() }
