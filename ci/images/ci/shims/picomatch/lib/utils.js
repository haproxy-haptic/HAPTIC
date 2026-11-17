'use strict';

const path = require('path');
const win32 = process.platform === 'win32';
const {
  REGEX_BACKSLASH,
  REGEX_SPECIAL_CHARS,
  REGEX_SPECIAL_CHARS_GLOBAL
} = require('./constants');

exports.isObject = val => val !== null && typeof val === 'object' && !Array.isArray(val);
exports.hasRegexChars = str => REGEX_SPECIAL_CHARS.test(str);
exports.isRegexChar = str => str.length === 1 && exports.hasRegexChars(str);
exports.escapeRegex = str => str.replace(REGEX_SPECIAL_CHARS_GLOBAL, '\\$1');
exports.toPosixSlashes = str => str.replace(REGEX_BACKSLASH, '/');

// Linear rewrite of upstream's str.replace(REGEX_REMOVE_BACKSLASH, ...), which
// backtracks polynomially on many '['; the equivalence test pins identical output.
const isLineTerminator = ch => ch === '\n' || ch === '\r' || ch === '\u2028' || ch === '\u2029';

exports.removeBackslashes = str => {
  const n = str.length;
  const nextClose = new Array(n + 2).fill(-1);
  const nextLineTerminator = new Array(n + 2).fill(n);
  for (let k = n - 1; k >= 0; k--) {
    nextClose[k] = k >= 1 && str[k] === ']' && str[k - 1] !== '\\' ? k : nextClose[k + 1];
    nextLineTerminator[k] = isLineTerminator(str[k]) ? k : nextLineTerminator[k + 1];
  }
  let out = '';
  let i = 0;
  while (i < n) {
    if (str[i] === '[' && i + 2 < n) {
      const close = nextClose[i + 2];
      if (close !== -1 && nextLineTerminator[i + 1] >= close - 1) {
        out += str.slice(i, close + 1);
        i = close + 1;
        continue;
      }
    }
    if (str[i] === '\\' && i + 1 < n && !isLineTerminator(str[i + 1])) {
      i++;
      continue;
    }
    out += str[i];
    i++;
  }
  return out;
};

exports.supportsLookbehinds = () => {
  const segs = process.version.slice(1).split('.').map(Number);
  if (segs.length === 3 && segs[0] >= 9 || (segs[0] === 8 && segs[1] >= 10)) {
    return true;
  }
  return false;
};

exports.isWindows = options => {
  if (options && typeof options.windows === 'boolean') {
    return options.windows;
  }
  return win32 === true || path.sep === '\\';
};

exports.escapeLast = (input, char, lastIdx) => {
  const idx = input.lastIndexOf(char, lastIdx);
  if (idx === -1) return input;
  if (input[idx - 1] === '\\') return exports.escapeLast(input, char, idx - 1);
  return `${input.slice(0, idx)}\\${input.slice(idx)}`;
};

exports.removePrefix = (input, state = {}) => {
  let output = input;
  if (output.startsWith('./')) {
    output = output.slice(2);
    state.prefix = './';
  }
  return output;
};

exports.wrapOutput = (input, state = {}, options = {}) => {
  const prepend = options.contains ? '' : '^';
  const append = options.contains ? '' : '$';

  let output = `${prepend}(?:${input})${append}`;
  if (state.negated === true) {
    output = `(?:^(?!${output}).*$)`;
  }
  return output;
};
