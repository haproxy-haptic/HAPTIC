// Playground editors: CodeMirror 6 (vendored, no CDN). The config editor's
// highlighting comes from the SAME Lezer grammar the docs facade uses
// (./highlight/config-highlight.bundle.js) — one grammar, both surfaces, so the
// editor and the shown config match by construction. The vendored bundle is
// imported dynamically inside makeEditors so that if it ever fails to load, we
// fall back to a plain <textarea> and the playground still works.

import { configParser, templateParser, highlighter } from './highlight/config-highlight.bundle.js';

/* ---------- Scriggo template grammar (CM-independent data) ---------- */
// Delimiter forms: {{ … }}, {% … %} (with {%- / -%} trim), {%% … %%} (blocks),
// and {# … #} comments. Match the block form first so its %%} isn't mis-closed
// by the {% … %} rule. Highlighting is done by the Lezer grammar now; this
// regex only survives for go-to-definition (tokenAt: is the cursor in a tag?).
const TPL_RE = /\{%%[\s\S]*?%%\}|\{%[\s\S]*?%\}|\{\{[\s\S]*?\}\}|\{#[\s\S]*?#\}/g;

/* ---------- HAProxy config grammar (for the rendered output pane) ---------- */
// Section keywords that open an HAProxy section (highlighted as headings). Every
// other leading word of a config line is treated as a directive keyword.
const HP_SECTIONS = new Set(['global', 'defaults', 'frontend', 'backend', 'listen',
  'peers', 'resolvers', 'cache', 'ring', 'userlist', 'mailers', 'program',
  'http-errors', 'fcgi-app', 'log-forward', 'crt-store', 'traces', 'acme', 'ruleset']);

// haproxyTokens classifies one HAProxy config line into spans, each
// {start, end, cls} with cls one of sec|kw|fn|num|str|cmt. The rendered output
// pane (index.html hlHaproxy) tokenizes the final haproxy.cfg with it; the
// editor's own highlighting comes from the Lezer grammar.
export function haproxyTokens(line) {
  const toks = [];
  // haproxy comments run '#' to end of line.
  let code = line, commentStart = -1;
  const hash = line.indexOf('#');
  if (hash >= 0) { code = line.slice(0, hash); commentStart = hash; }
  const lead = code.match(/^\s*/)[0];
  const rest = code.slice(lead.length);
  if (rest) {
    const parts = rest.split(/(\s+)/);   // keep the whitespace runs to track offsets
    let pos = lead.length, firstWord = true, afterSection = false;
    for (const tok of parts) {
      const start = pos; pos += tok.length;
      if (tok === '' || /^\s+$/.test(tok)) continue;
      if (firstWord) {
        firstWord = false;
        if (lead === '' && HP_SECTIONS.has(tok.toLowerCase())) { toks.push({ start, end: pos, cls: 'sec' }); afterSection = true; }
        else toks.push({ start, end: pos, cls: 'kw' });
      } else if (afterSection) {
        afterSection = false;
        toks.push({ start, end: pos, cls: 'fn' });   // proxy name after a section keyword
      } else if (/^-?\d[\w.:/]*$/.test(tok)) {
        toks.push({ start, end: pos, cls: 'num' });   // numbers, status codes, ports, IPs
      } else if (/^["']/.test(tok)) {
        toks.push({ start, end: pos, cls: 'str' });
      }
    }
  }
  if (commentStart >= 0) toks.push({ start: commentStart, end: line.length, cls: 'cmt' });
  return toks;
}

/* ---------- autocomplete data ---------- */
// Builtins + runtime context vars, mirrored from pkg/templating (filters_scriggo.go
// constants + registerScriggoRuntimeVars). Convenience data, not a correctness path.
const FUNCTIONS = ['b64decode', 'debug', 'glob_match', 'indent', 'sort_by', 'strip', 'toJSON',
  'trim', 'append', 'basename', 'ceil', 'coalesce', 'condition', 'dig', 'dig_string', 'fail',
  'fallback', 'first_seen', 'isdigit', 'isNil', 'join', 'join_key', 'keys', 'make_guid', 'merge',
  'namespace', 'on_own_lines', 'recordEvent', 'regex_search', 'replace', 'sanitize_regex', 'selectattr', 'semver_gte', 'seq',
  'shard_slice', 'sort_ints', 'sort_strings', 'statusPatch', 'strings_contains', 'strings_lower',
  'strings_splitn', 'strings_split', 'strings_trim', 'title', 'tofloat', 'toint',
  'toSlice', 'toStringSlice', 'tostring', 'to_str_map', 'transitionTime'];
const VARS = ['resources', 'pathResolver', 'controller', 'templateSnippets', 'fileRegistry',
  'dataplane', 'capabilities', 'shared', 'extraContext', 'http', 'runtimeEnvironment', 'currentConfig'];
const KEYWORDS = ['for', 'in', 'if', 'else', 'end', 'range', 'var', 'show', 'break', 'continue',
  'return', 'switch', 'case', 'default', 'macro', 'extends', 'using', 'defer', 'and', 'or', 'not', 'contains'];

const IDENT_OPTS = [
  ...KEYWORDS.map((k) => ({ label: k, type: 'keyword' })),
  ...VARS.map((v) => ({ label: v, type: 'variable', detail: 'context' })),
  ...FUNCTIONS.map((f) => ({ label: f, type: 'function', detail: 'builtin', apply: f + '(' })),
];

// Typed-field completion: property names from the loaded OpenAPI schema bundle,
// offered inside dig(subject, "a", "b", …) string arguments. fieldsAtPath(segs)
// returns the union of child field names reachable after the already-typed path
// segments, across ALL watched-resource schemas — path-aware but resource-agnostic
// (the dig subject's concrete type isn't tracked). null until a schema is loaded.
let fieldsAtPath = null;

export function setSchemaBundle(json) {
  fieldsAtPath = null;
  if (!json) return;
  let bundle;
  try { bundle = JSON.parse(json); } catch { return; }
  const schemas = Object.values(bundle).map((e) => e && e.schema).filter(Boolean);
  if (!schemas.length) return;
  // The child-property map of a schema node: object.properties, or array items.properties.
  const childrenOf = (node) => {
    if (!node || typeof node !== 'object') return null;
    if (node.properties) return node.properties;
    if (node.items && node.items.properties) return node.items.properties;
    return null;
  };
  const walk = (node, segs) => {
    let p = childrenOf(node);
    for (const s of segs) { if (!p || !p[s]) return null; p = childrenOf(p[s]); }
    return p;
  };
  fieldsAtPath = (segs) => {
    const out = new Set();
    if (!segs.length) {                        // no path yet → resource roots (apiVersion/spec/…)
      for (const sc of schemas) { const p = childrenOf(sc); if (p) Object.keys(p).forEach((k) => out.add(k)); }
      return [...out];
    }
    // Match the segment sequence starting at ANY node (handles digging from a
    // nested subject, e.g. dig(rule, "backend", …)), unioning the children.
    const stack = schemas.slice(), seen = new Set();
    while (stack.length) {
      const n = stack.pop();
      if (!n || typeof n !== 'object' || seen.has(n)) continue;
      seen.add(n);
      const f = walk(n, segs); if (f) Object.keys(f).forEach((k) => out.add(k));
      const p = childrenOf(n); if (p) for (const k in p) stack.push(p[k]);
    }
    return [...out];
  };
}

// digContext returns the argument text (from the call's "(" to the cursor) when
// the cursor sits inside a dig()/dig_string() call, else null. Scans backwards
// tracking paren depth so nested balanced calls (…List()) don't fool it.
function digContext(before) {
  let depth = 0;
  for (let i = before.length - 1; i >= 0; i--) {
    const c = before[i];
    if (c === ')') depth++;
    else if (c === '(') {
      if (depth === 0) return /(?:\bdig_string|\bdig)\s*$/.test(before.slice(0, i)) ? before.slice(i + 1) : null;
      depth--;
    }
  }
  return null;
}

// digFieldOptions returns { partial, fields } for the dig() string argument the
// cursor is in — `partial` is the text already typed in the current segment,
// `fields` the sorted schema field names to offer — or null when the cursor
// isn't inside an open dig-string argument. Exported for unit testing without
// CodeMirror; templateCompletions wraps it into a CM completion result.
export function digFieldOptions(before) {
  if (!fieldsAtPath) return null;
  const args = digContext(before);
  if (args == null) return null;
  // Odd number of quotes → the cursor sits inside an unterminated string (the
  // current path segment). Even → every string is closed, so we're between args.
  if ((args.match(/['"]/g) || []).length % 2 === 0) return null;
  const qi = Math.max(args.lastIndexOf('"'), args.lastIndexOf("'")); // the open quote
  const prior = [...args.slice(0, qi).matchAll(/(['"])(.*?)\1/g)].map((m) => m[2]);
  const fields = fieldsAtPath(prior);
  return fields.length ? { partial: args.slice(qi + 1), fields: fields.slice().sort() } : null;
}

// Watched-resource keys declared in the current config's `watchedResources:` block.
function watchedKeys(doc) {
  const text = doc.toString();
  const start = text.search(/^watchedResources:/m);
  if (start < 0) return [];
  const keys = [];
  for (const line of text.slice(start).split('\n').slice(1)) {
    if (/^\S/.test(line)) break;                  // dedented to the next top-level key
    const mm = line.match(/^  (\w[\w-]*):\s*$/);  // two-space-indented map key
    if (mm) keys.push(mm[1]);
  }
  return keys;
}

function insideTemplate(before) {
  const open = Math.max(before.lastIndexOf('{{'), before.lastIndexOf('{%'));
  const close = Math.max(before.lastIndexOf('}}'), before.lastIndexOf('%}'));
  return open > close;
}

function templateCompletions(context) {
  const before = context.state.doc.sliceString(Math.max(0, context.pos - 4000), context.pos);
  if (!insideTemplate(before)) return null;

  // Typed-field completion inside dig(subject, "seg", "seg", "<here>").
  const dig = digFieldOptions(before);
  if (dig) {
    return {
      from: context.pos - dig.partial.length,
      options: dig.fields.map((f) => ({ label: f, type: 'property', detail: 'field' })),
      validFor: /^[\w.-]*$/,
    };
  }

  const dotted = context.matchBefore(/[\w.]+/);
  if (dotted && dotted.text.includes('.')) {
    const seg = context.matchBefore(/\w*/);        // segment after the last dot
    const head = dotted.text.slice(0, dotted.text.length - seg.text.length).replace(/\.$/, '');
    const options = head === 'resources'
      ? watchedKeys(context.state.doc).map((k) => ({ label: k, type: 'class', detail: 'watched' }))
      : [{ label: 'List', type: 'method', detail: '() []any', apply: 'List()' }];  // resources.<kind>.List()
    return { from: seg.from, options, validFor: /^\w*$/ };
  }
  const word = context.matchBefore(/\w*/);
  if (!word || (word.from === word.to && !context.explicit)) return null;
  return { from: word.from, options: IDENT_OPTS, validFor: /^\w*$/ };
}

/* ---------- CM-dependent setup (built after the bundle loads) ---------- */
function initCodeMirror(CM, scriggoMode) {
  const { EditorView, EditorState, basicSetup, Prec, syntaxHighlighting, LRLanguage, LanguageSupport, yaml, autocompletion, MergeView } = CM;

  // The config editor is highlighted by the shared Lezer grammar: configParser
  // for a full HAProxyTemplateConfig, templateParser for a bare scriggo
  // scratchpad (?scriggo=1). `highlighter` maps grammar tags to the pg-cf-*
  // classes styled in index.html — the same classes the docs facade uses.
  const configLang = new LanguageSupport(LRLanguage.define({ parser: configParser }));
  const templateLang = new LanguageSupport(LRLanguage.define({ parser: templateParser }));
  const editorLang = scriggoMode ? templateLang : configLang;

  // Editor theme — a terminal, always dark regardless of page theme.
  const theme = EditorView.theme({
    '&': { height: '100%', backgroundColor: 'transparent', color: 'var(--term-fg)' },
    '.cm-scroller': { fontFamily: 'var(--mono)', fontSize: '12.5px', lineHeight: '1.6' },
    '.cm-content': { padding: '10px 0' },
    '.cm-gutters': { backgroundColor: 'transparent', color: 'var(--term-dim)', border: 'none' },
    '.cm-activeLine': { backgroundColor: 'rgba(107,163,214,.06)' },
    '.cm-activeLineGutter': { backgroundColor: 'transparent', color: 'var(--term-fg)' },
    '.cm-cursor': { borderLeftColor: 'var(--accent-2)' },
    '.cm-selectionBackground, &.cm-focused .cm-selectionBackground': { backgroundColor: 'color-mix(in srgb, var(--accent) 24%, transparent)' },
    '&.cm-focused': { outline: 'none' },
    '.cm-tooltip': { backgroundColor: 'var(--term-chrome)', border: '1px solid var(--term-border)', borderRadius: '8px', color: 'var(--term-fg)' },
    '.cm-tooltip.cm-tooltip-autocomplete > ul': { fontFamily: 'var(--mono)', fontSize: '12px', maxHeight: '16em' },
    '.cm-tooltip-autocomplete ul li[aria-selected]': { backgroundColor: 'var(--accent-soft)', color: 'var(--term-fg)' },
    '.cm-completionLabel': { color: 'var(--term-fg)' },
    '.cm-completionDetail': { color: 'var(--term-dim)', fontStyle: 'normal', marginLeft: '1em' },
    '.cm-completionIcon': { color: 'var(--accent)', opacity: '.8' },
    // Ctrl/Cmd-hover go-to-definition affordance.
    '.cm-godef': { textDecoration: 'underline', textDecorationColor: 'var(--accent)', textUnderlineOffset: '2px', cursor: 'pointer' },
  }, { dark: true });

  const base = [
    basicSetup,
    Prec.highest(syntaxHighlighting(highlighter)),   // our palette wins over basicSetup's default
    theme,
    EditorView.lineWrapping,
  ];
  // The config editor gets the composite grammar + template-aware completions;
  // the resources editor is plain YAML (its own language, so it isn't parsed as
  // a HAProxyTemplateConfig). Both share the highlighter above.
  //
  // templateCompletions is registered as an `override` source, not via
  // language.data: the caret lives inside a parseMixed mount (the template block
  // scalar), and CM6 does NOT fall back to the outer language's data across a
  // mount boundary, so a language-scoped source never fires there. The source
  // self-gates on insideTemplate(), so being global is safe.
  const configExtra = [editorLang, autocompletion({ override: [templateCompletions] })];
  const resExtra = [yaml()];
  // Side-by-side read-only diff of two configs (the challenge "compare with
  // solution" view). Same language + palette as the live editor, so the diff
  // reads like the editor. Returns the MergeView (caller calls .destroy()).
  const makeMerge = (parent, aDoc, bDoc) => {
    const ro = [
      basicSetup, editorLang,
      Prec.highest(syntaxHighlighting(highlighter)),
      theme, EditorView.lineWrapping,
      EditorState.readOnly.of(true), EditorView.editable.of(false),
    ];
    return new MergeView({ parent, a: { doc: aDoc, extensions: ro }, b: { doc: bDoc, extensions: ro } });
  };
  return { EditorView, base, configExtra, resExtra, makeMerge };
}

/* ---------- public API ---------- */
export async function makeEditors(onConfigChange, onResChange, onNav, onGotoDef, scriggoMode) {
  const $ = (id) => document.getElementById(id);
  const notify = (name) => { if (onNav) onNav(name); };
  try {
    const CM = await import('./vendor/codemirror.js');
    const { EditorView, base, configExtra, resExtra, makeMerge } = initCodeMirror(CM, scriggoMode);
    const { Decoration, ViewPlugin } = CM;
    // The token under a document position: word run (including '-' for HAProxy
    // keywords like http-request, '.' for Scriggo member chains like
    // pathResolver.GetPath), trimmed of leading/trailing punctuation. Returns the
    // trimmed token plus its document offsets so both the click and the hover
    // affordance resolve the exact same span.
    function tokenAt(view, pos) {
      const doc = view.state.doc, line = doc.lineAt(pos), col = pos - line.from, lt = line.text;
      let inTpl = false, m; TPL_RE.lastIndex = 0;
      const text = doc.toString();
      while ((m = TPL_RE.exec(text))) { if (pos >= m.index && pos < m.index + m[0].length) { inTpl = true; break; } }
      const ch = inTpl ? /[\w.]/ : /[\w-]/;
      let a = col, b = col;
      while (a > 0 && ch.test(lt[a - 1])) a--;
      while (b < lt.length && ch.test(lt[b])) b++;
      while (a < b && /[.-]/.test(lt[a])) a++;
      while (b > a && /[.-]/.test(lt[b - 1])) b--;
      const token = lt.slice(a, b);
      return token ? { token, inTpl, from: line.from + a, to: line.from + b } : null;
    }

    // Ctrl/Cmd-hover feedback: underline the token a Ctrl/Cmd-click would open.
    // A ViewPlugin renders the mark from `hoverTarget`; the mouse handlers update
    // `hoverTarget` and force a redraw with an empty dispatch (only when it changes,
    // so mousemove doesn't spam transactions).
    let hoverTarget = null, godefView = null;
    const godefMark = Decoration.mark({ class: 'cm-godef' });
    const godefPlugin = ViewPlugin.fromClass(class {
      constructor() { this.decorations = this.build(); }
      update() { this.decorations = this.build(); }
      build() { return hoverTarget ? Decoration.set([godefMark.range(hoverTarget.from, hoverTarget.to)]) : Decoration.none; }
    }, { decorations: (v) => v.decorations });
    const setHover = (view, next) => {
      const cur = hoverTarget;
      if ((!cur && !next) || (cur && next && cur.from === next.from && cur.to === next.to)) return;
      hoverTarget = next;
      view.dispatch({});   // no-op tx → godefPlugin.update() re-reads hoverTarget
    };

    // Ctrl/Cmd + left-click → "go to definition": open the matching HAProxy / HAPTIC docs.
    const gotoDefExt = EditorView.domEventHandlers({
      mousedown(e, view) {
        if (!onGotoDef || e.button !== 0 || !(e.ctrlKey || e.metaKey)) return false;
        const pos = view.posAtCoords({ x: e.clientX, y: e.clientY });
        const t = pos == null ? null : tokenAt(view, pos);
        if (!t) return false;
        e.preventDefault();
        onGotoDef({ token: t.token, scriggo: t.inTpl });
        return true;
      },
      mousemove(e, view) {
        godefView = view;
        if (!(e.ctrlKey || e.metaKey)) { setHover(view, null); return false; }
        const pos = view.posAtCoords({ x: e.clientX, y: e.clientY });
        const t = pos == null ? null : tokenAt(view, pos);
        setHover(view, t ? { from: t.from, to: t.to } : null);
        return false;
      },
      mouseleave(e, view) { setHover(view, null); return false; },
    });
    // Clear the hover underline when Ctrl/Cmd is released, even if the editor
    // isn't focused (CM's own keyup only fires while focused).
    document.addEventListener('keyup', (e) => { if (!(e.ctrlKey || e.metaKey) && godefView) setHover(godefView, null); });
    const mk = (parent, extra, cb, name) => {
      parent.innerHTML = '';
      const view = new EditorView({
        parent, doc: '',
        extensions: [...base, ...extra, EditorView.updateListener.of((u) => { if (u.docChanged) cb(); })],
      });
      // Cursor+scroll history with browser-style back/forward: a jump (goto)
      // pushes where we were onto `back` and clears `fwd` (a new branch); back
      // moves current→fwd and restores from back; forward is the mirror. Cleared
      // on a full document replace, where old offsets no longer mean anything.
      const back = [], fwd = [];
      const snapshot = () => ({ anchor: view.state.selection.main.anchor, scrollTop: view.scrollDOM.scrollTop });
      const restore = (p) => {
        view.dispatch({ selection: { anchor: Math.min(p.anchor, view.state.doc.length) } });
        view.focus();
        // Restore the exact prior scroll, overriding the selection's auto-scroll.
        view.requestMeasure({ read: () => {}, write: () => { view.scrollDOM.scrollTop = p.scrollTop; } });
      };
      const step = (from, to) => { const p = from.pop(); if (!p) return false; to.push(snapshot()); restore(p); notify(name); return true; };
      return {
        get: () => view.state.doc.toString(),
        set: (t) => { back.length = 0; fwd.length = 0; view.dispatch({ changes: { from: 0, to: view.state.doc.length, insert: t } }); notify(name); },
        goto: (line, col) => {
          back.push(snapshot());
          if (back.length > 200) back.shift();
          fwd.length = 0;
          const l = Math.max(1, Math.min(line | 0, view.state.doc.lines));
          const lo = view.state.doc.line(l);
          const pos = Math.min(lo.from + Math.max(0, (col | 0) - 1), lo.to);
          view.dispatch({ selection: { anchor: pos } });
          view.focus();
          // Center the line in the viewport. scrollIntoView({y:'center'}) is
          // unreliable here (the selection's own scroll wins), so set scrollTop
          // directly from the line's block geometry. Deferred to after layout.
          view.requestMeasure({
            read: () => view.lineBlockAt(pos),
            write: (block) => {
              const sc = view.scrollDOM;
              sc.scrollTop = Math.max(0, block.top - sc.clientHeight / 2 + block.height / 2);
            },
          });
          notify(name);
        },
        back: () => step(back, fwd),
        forward: () => step(fwd, back),
        canBack: () => back.length > 0,
        canForward: () => fwd.length > 0,
      };
    };
    return {
      config: mk($('ed-config'), [...configExtra, gotoDefExt, godefPlugin], onConfigChange, 'config'),
      res: mk($('ed-res'), resExtra, onResChange, 'res'),
      kind: 'codemirror',
      makeMerge: makeMerge,   // (parent, aDoc, bDoc) -> MergeView; solution-compare view
    };
  } catch (err) {
    console.warn('CodeMirror unavailable, using textareas:', err);
    const mk = (parent, ph, cb, name) => {
      parent.innerHTML = '';
      const ta = document.createElement('textarea');
      ta.className = 'fallback'; ta.spellcheck = false; ta.placeholder = ph;
      ta.addEventListener('input', cb); parent.appendChild(ta);
      const back = [], fwd = [];
      const snapshot = () => ({ start: ta.selectionStart, scrollTop: ta.scrollTop });
      const restore = (p) => { ta.focus(); ta.setSelectionRange(p.start, p.start); ta.scrollTop = p.scrollTop; };
      const step = (from, to) => { const p = from.pop(); if (!p) return false; to.push(snapshot()); restore(p); notify(name); return true; };
      const offOf = (line, col) => {
        const rows = ta.value.split('\n');
        const l = Math.max(1, Math.min(line | 0, rows.length));
        let off = 0;
        for (let i = 0; i < l - 1; i++) off += rows[i].length + 1;
        return off + Math.max(0, (col | 0) - 1);
      };
      return {
        get: () => ta.value,
        set: (t) => { back.length = 0; fwd.length = 0; ta.value = t; notify(name); },
        goto: (line, col) => {
          back.push(snapshot());
          if (back.length > 200) back.shift();
          fwd.length = 0;
          const off = offOf(line, col);
          ta.focus(); ta.setSelectionRange(off, off);
          notify(name);
        },
        back: () => step(back, fwd),
        forward: () => step(fwd, back),
        canBack: () => back.length > 0,
        canForward: () => fwd.length > 0,
      };
    };
    return {
      config: mk($('ed-config'), '', onConfigChange, 'config'),
      res: mk($('ed-res'), 'paste `kubectl get ingress,svc -A -o yaml` here…', onResChange, 'res'),
      kind: 'textarea',
    };
  }
}
