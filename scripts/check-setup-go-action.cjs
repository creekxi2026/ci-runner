// Execute the distributed action and validate runner commands, not just Node's
// exit status. GitHub can fail a step when an add-matcher command references a
// missing file even though the JavaScript process itself exits successfully.
const fs = require('fs');
const os = require('os');
const path = require('path');
const assert = require('assert/strict');
const {spawnSync} = require('child_process');
const action = path.resolve(__dirname, '../actions/setup-go');
const fixture = fs.mkdtempSync(path.join(os.tmpdir(), 'setup-go-contract-'));

function execute(directory, name) {
  const home = path.join(fixture, name);
  fs.mkdirSync(home);
  for (const file of ['env', 'output', 'path']) fs.writeFileSync(path.join(home, file), '');
  const result = spawnSync(process.execPath, [path.join(directory, 'dist/setup/index.js')], {
    encoding: 'utf8', timeout: 30000,
    env: {...process.env, HOME:home, RUNNER_TEMP:home,
      'INPUT_GO-VERSION':'1.26.6', 'INPUT_CHECK-LATEST':'false', INPUT_CACHE:'false', INPUT_TOKEN:'',
      GITHUB_ENV:path.join(home,'env'), GITHUB_OUTPUT:path.join(home,'output'), GITHUB_PATH:path.join(home,'path')}
  });
  assert.equal(result.status, 0, 'action process must succeed with the preinstalled toolchain');
  assert.match(result.stdout, /Found in cache @/);
  const matchers = [...result.stdout.matchAll(/^(?:##\[add-matcher\]|::add-matcher::)([^\r\n]+)$/gm)];
  assert.equal(matchers.length, 1, 'action must register its Go error matcher');
  for (const [,file] of matchers) {
    const data = JSON.parse(fs.readFileSync(file, 'utf8'));
    assert.equal(data.problemMatcher.length, 1);
    const pattern = data.problemMatcher[0].pattern[0];
    const match = new RegExp(pattern.regexp).exec('main.go:3:1: undefined: missing');
    assert.equal(match[pattern.file], 'main.go');
    assert.equal(match[pattern.line], '3');
    assert.equal(match[pattern.column], '1');
    assert.equal(match[pattern.message], 'undefined: missing');
  }
}

try {
  execute(action, 'complete');
  const missing = path.join(fixture, 'missing');
  fs.mkdirSync(path.join(missing, 'dist/setup'), {recursive:true});
  fs.copyFileSync(path.join(action, 'dist/setup/index.js'), path.join(missing, 'dist/setup/index.js'));
  assert.throws(() => execute(missing, 'incomplete'), {code:'ENOENT'});
  console.log('Distributed setup-go: toolcache hit, matcher registered and Go diagnostics parsed; missing asset regression rejected.');
} finally {
  fs.rmSync(fixture, {recursive:true, force:true});
}
