const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const source = readFileSync(join(__dirname, '../update-transfer.js'), 'utf8');
const id = 'a'.repeat(32);
function setup(offset=0) {
  const calls=[], progress=[], retries=[];
  let failures=0, error='NETWORK';
  const state=()=>({state:'uploading',uploadId:id,offset,chunkSize:200});
  const ctx=vm.createContext({Uint8Array, AbortController, setTimeout:fn=>setTimeout(fn,0), clearTimeout,
    Http:{failure:(code,message)=>Object.assign(new Error(message),{code}),
      request:async(method,url,{body})=>{calls.push([url,body]);return url.endsWith('start')?state():{state:'validating'};},
      upload:async(url,blob,{headers})=>{calls.push([url,headers,blob.size]);
        if(failures-->0) throw Object.assign(new Error(error),{code:error});
        offset=Number(headers['Upload-Offset'])+blob.size;return state();}}});
  vm.runInContext(source,ctx);
  const file=new Blob([new Uint8Array(500)]);file.name='update.rvu';
  return {calls,progress,retries,file,fail:(n,code='NETWORK')=>{failures=n;error=code;},
    upload:options=>vm.runInContext('UpdateTransfer',ctx).upload(file,{onProgress:p=>progress.push(p),onRetry:n=>retries.push(n),...options})};
}
test('progress counts acknowledged bytes and finishes only after all chunks',async()=>{
  const f=setup();assert.equal((await f.upload()).state,'validating');
  assert.deepEqual(f.progress,[0,40,80,100]);
  assert.deepEqual(f.calls.filter(c=>c[0].endsWith('chunk')).map(c=>c[2]),[200,200,100]);
  assert.equal(f.calls[0][1].header.length,56);assert.equal(f.calls[0][1].seal.length,192);
});
test('resume skips persisted bytes; transient failure retries identical offset',async()=>{
  const f=setup(200);f.fail(1);await f.upload();
  assert.deepEqual(f.progress,[40,80,100]);assert.deepEqual(f.retries,[1]);
  const chunks=f.calls.filter(c=>c[0].endsWith('chunk'));
  assert.equal(chunks[0][1]['Upload-Offset'],'200');assert.equal(chunks[1][1]['Upload-Offset'],'200');
});
test('retry bounded; authorization failures never retried',async()=>{
  const f=setup();f.fail(10);await assert.rejects(f.upload(),{code:'NETWORK'});
  assert.equal(f.calls.filter(c=>c[0].endsWith('chunk')).length,4);
  assert.deepEqual(f.progress,[0]);
  const g=setup();g.fail(10,'AUTH_REQUIRED');await assert.rejects(g.upload(),{code:'AUTH_REQUIRED'});
  assert.equal(g.retries.length,0);
});
test('abort and plaintext extension rejected without network',async()=>{
  const f=setup();const controller=new AbortController();controller.abort();
  await assert.rejects(f.upload({signal:controller.signal}),{code:'ABORTED'});assert.equal(f.calls.length,0);
  f.file.name='bad.zip';await assert.rejects(f.upload(),{code:'UPDATE_SIGNATURE_REQUIRED'});assert.equal(f.calls.length,0);
});
