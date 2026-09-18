// Synthetic DOM only: no browser, network, real IDs/tokens, HTML or page data.
const fs = require('fs');
const vm = require('vm');
const input = JSON.parse(fs.readFileSync(0, 'utf8'));

class Element {
  constructor(tag, classes = [], attrs = {}, children = [], hidden = false) {
    Object.assign(this, { tag, classes, attrs, children, hidden });
    for (const child of children) child.parent = this;
  }
  getAttribute(name) {
    if (name !== 'href') throw Error('unexpected attribute read');
    return this.attrs[name] ?? null;
  }
  get textContent() { throw Error('text must not be read'); }
  get innerHTML() { throw Error('HTML must not be read'); }
  get outerHTML() { throw Error('HTML must not be read'); }
  getBoundingClientRect() {
    let hidden = false;
    for (let e = this; e; e = e.parent) hidden ||= e.hidden;
    return { width: hidden ? 0 : 100, height: hidden ? 0 : 100 };
  }
  querySelectorAll(selector) {
    const selectors = selector.split(',').map(s => s.trim().replace(/>/g, ' > ').split(/\s+/));
    const out = [];
    const visit = node => {
      for (const child of node.children) {
        if (selectors.some(parts => matchesChain(child, parts, parts.length - 1))) out.push(child);
        visit(child);
      }
    };
    visit(this);
    return out;
  }
}
function matchesSimple(node, selector) {
  const tag = selector.match(/^[a-z]+/);
  if (tag && node.tag !== tag[0]) return false;
  for (const [, cls] of selector.matchAll(/\.([\w-]+)/g)) if (!node.classes.includes(cls)) return false;
  for (const [, attr] of selector.matchAll(/\[([\w-]+)\]/g)) if (!(attr in node.attrs)) return false;
  return true;
}
function matchesChain(node, parts, at) {
  if (!node || !matchesSimple(node, parts[at])) return false;
  if (at === 0) return true;
  if (parts[at - 1] === '>') return matchesChain(node.parent, parts, at - 2);
  for (let p = node.parent; p; p = p.parent) if (matchesChain(p, parts, at - 1)) return true;
  return false;
}
function card(c) {
  const links = (c.links || []).map(entry => {
    const e = typeof entry === 'string' ? { href: entry } : entry;
    return new Element('a', e.classes ?? ['cover', 'mask', 'ld'], { href: e.href }, [], !!e.hidden);
  });
  // Production card has a div wrapper between section and its anchors.
  return new Element(c.tag ?? 'section', c.classes ?? ['note-item'], {}, [new Element('div', [], {}, links)], !!c.hidden);
}
const container = new Element('div', input.containerClasses ?? ['feeds-container'], {}, (input.cards || []).map(card));
const area = new Element('div', input.areaClasses ?? ['search-layout__main'], {}, [container]);
const outside = (input.outsideCards || [{ links: ['/search_result/outside-fixture?xsec_token=synthetic-outside'] }]).map(card);
const document = new Element('document', [], {}, [area, ...outside]);
const context = vm.createContext({
  URL, document,
  window: { location: { origin: 'https://www.xiaohongshu.com' }, __INITIAL_STATE__: { search: { feeds: input.feeds } } },
  getComputedStyle: () => ({ display: 'block', visibility: 'visible' }),
});
process.stdout.write(JSON.stringify(vm.runInContext(input.script, context, { timeout: 1000 })));
