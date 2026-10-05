const fs = require('fs');
const path = require('path');
const {spawnSync} = require('child_process');
const bundle = fs.readFileSync(path.join(__dirname, '../actions/setup-go/dist/setup/index.js'), 'utf8');
const begin = bundle.indexOf('/***/ 75067:');
const helperStart = bundle.indexOf('var __awaiter =', begin);
const helperEnd = bundle.indexOf('Object.defineProperty(exports', helperStart);
const start = bundle.indexOf('const promiseWithTimeout =', begin);
const end = bundle.indexOf('//# sourceMappingURL=downloadUtils.js.map', start);
if ([begin, helperStart, helperEnd, start, end].some(n => n < 0)) throw Error('upstream module changed');
const fn = bundle.slice(helperStart, helperEnd) + bundle.slice(start, end);
if (!fn.includes('.finally(')) throw Error('timer cleanup missing');
const results = [];
for (const mode of ['reject', 'resolve', 'timeout']) {
  const child = `${fn}\nconst begin=Date.now(); const failure=new Error('injected transport abort');
const operation = new Promise((resolve,reject)=>{if('${mode}'!=='timeout')setTimeout(()=>'${mode}'==='reject'?reject(failure):resolve('ok'),20)});
promiseWithTimeout(800,operation).then(value=>console.log(JSON.stringify({value})),error=>{
 if(error!==failure)process.exitCode=1;
 console.log(JSON.stringify({error:error.message}));
});
process.on('beforeExit',()=>console.log(JSON.stringify({exitMs:Date.now()-begin})));`;
  const result = spawnSync(process.execPath, ['-e', child], {encoding:'utf8', timeout:3000});
  if (result.status !== 0) throw Error(result.stderr || String(result.error));
  const values = Object.assign({}, ...result.stdout.trim().split('\n').map(JSON.parse));
  if (mode === 'timeout' ? values.value !== 'timeout' || values.exitMs < 750 : values.exitMs > 500) throw Error(JSON.stringify(values));
  if (mode === 'resolve' && values.value !== 'ok') throw Error('success changed');
  if (mode === 'reject' && values.error !== 'injected transport abort') throw Error('rejection changed');
  results.push({mode, ...values});
}
console.log(JSON.stringify({node:process.version,results},null,2));
