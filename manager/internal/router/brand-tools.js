// SPDX-License-Identifier: MIT

// OpenCode 2.2 renders MCP IDs instead of discovery titles. Change only its
// tool heading text and accessible labels; preserve DOM nodes owned by Solid.
(() => {
  const selector = '[data-slot="basic-tool-tool-title"]'
  const originals = new WeakMap()
  const dirty = new Set()
  let frame = 0
  let session = ''
  let externalTitles = {}
  let discovery
  const currentSession = () => location.pathname.match(/\/session\/(ses_[0-9A-Za-z]{8,80})(?:\/|$)/)?.[1] || ''
  const refreshScope = () => {
    const next = currentSession()
    if (next === session) return false
    session = next
    externalTitles = {}
    discovery?.abort()
    if (!next || typeof externalToolTitlesURL !== 'string') return true
    const request = new AbortController()
    discovery = request
    const signal = AbortSignal.any([request.signal, AbortSignal.timeout(10000)])
    fetch(`${externalToolTitlesURL}?session=${encodeURIComponent(next)}`, { credentials: 'same-origin', cache: 'no-store', signal })
      .then(response => response.ok ? response.json() : {})
      .then(labels => {
        if (request.signal.aborted || next !== currentSession() || !labels || typeof labels !== 'object' || Array.isArray(labels)) return
        externalTitles = Object.fromEntries(Object.entries(labels).filter(([key, value]) =>
          /^[A-Za-z0-9_-]{1,256}$/.test(key) && typeof value === 'string' && value.length <= 640))
        collect(document.documentElement)
        schedule()
      })
      .catch(() => {}) // Discovery failure leaves the original ID visible.
    return true
  }
  const translate = (node, attribute, labels) => {
    const current = attribute ? node.getAttribute(attribute) : node.nodeValue
    const previous = originals.get(node)
    const source = previous && current === previous.shown ? previous.source : current
    if (!source) return
    const shown = source.replace(/[A-Za-z0-9_-]+/g, name => Object.hasOwn(labels, name) ? labels[name] : name)
    originals.set(node, { source, shown })
    if (shown === current) return
    if (attribute) node.setAttribute(attribute, shown)
    else node.nodeValue = shown
  }
  const flush = () => {
    frame = 0
    const lang = (document.documentElement.lang || navigator.language).toLowerCase().startsWith('de') ? 'de' : 'en'
    const labels = { ...externalTitles, ...toolDisplayTitles[lang] }
    for (const heading of dirty) {
      if (!heading.isConnected) continue
      const walker = document.createTreeWalker(heading, NodeFilter.SHOW_TEXT)
      while (walker.nextNode()) translate(walker.currentNode, null, labels)
      for (const element of heading.querySelectorAll('[aria-label]')) translate(element, 'aria-label', labels)
      if (heading.hasAttribute('aria-label')) translate(heading, 'aria-label', labels)
    }
    dirty.clear()
  }
  const collect = node => {
    const element = node.nodeType === Node.ELEMENT_NODE ? node : node.parentElement
    if (!element) return
    const heading = element.closest(selector)
    if (heading) dirty.add(heading)
    for (const child of element.querySelectorAll(selector)) dirty.add(child)
  }
  const schedule = () => {
    if (dirty.size && !frame) frame = requestAnimationFrame(flush)
  }
  new MutationObserver(records => {
    if (refreshScope()) collect(document.documentElement)
    for (const record of records) {
      if (record.type === 'attributes' && record.attributeName === 'lang') collect(document.documentElement)
      else {
        // Do not rescan the transcript when unrelated message text streams in.
        const element = record.target.nodeType === Node.ELEMENT_NODE ? record.target : record.target.parentElement
        const heading = element?.closest(selector)
        if (heading) dirty.add(heading)
        for (const added of record.addedNodes) collect(added)
      }
    }
    schedule()
  }).observe(document.documentElement, { subtree: true, childList: true, characterData: true, attributes: true, attributeFilter: ['lang', 'aria-label'] })
  window.addEventListener('popstate', () => {
    if (refreshScope()) collect(document.documentElement)
    schedule()
  })
  refreshScope()
  collect(document.documentElement)
  schedule()
})()
