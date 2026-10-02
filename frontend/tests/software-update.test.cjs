const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const tick = () => new Promise(resolve=>setImmediate(resolve));
const source = readFileSync(join(__dirname,'../software-update.js'),'utf8');
function setup(pageVersion="1.1.2"){
 const nodes=new Map(),timers=new Map(),calls=[];
 const node = name => {
  if(!nodes.has(name)) nodes.set(name,{textContent:'',hidden:false,disabled:false,files:[],handlers:{},addEventListener(k,f){this.handlers[k]=f;},removeEventListener(k){delete this.handlers[k];}});
  return nodes.get(name);
 };
 const root={isConnected:true,querySelector:node},dialog={...node('dialog'),open:true};
 let html='',version=async()=>({version:'1.1.2'}),status={state:'idle'},apply=async()=>({state:'running'}),upload=async()=>({state:'ready',version:'1.1.2',ticket:'a'.repeat(32)});
 const ctx=vm.createContext({AbortController,Date,location:{reload(){calls.push('reload');}},document:{currentScript:{getAttribute:()=>`software-update.js?v=${pageVersion}`},hidden:false,querySelector:s=>s==='#dialog'?dialog:root},modal(title,body){html=body;dialog.open=true;},setTimeout(fn){const id=timers.size+1;timers.set(id,fn);return id;},clearTimeout(id){timers.delete(id);},Backend:{version:{get:opts=>version(opts)},softwareUpdate:{get:async()=>{if(status instanceof Error) throw status;return status;},apply:async opts=>{calls.push(opts);return apply(opts);}}},UpdateTransfer:{upload(file,options){calls.push([file,options]);return upload(file,options);}}});
 vm.runInContext(source,ctx);
 return {open:()=>vm.runInContext('SoftwareUpdate.open()',ctx),node,dialog,calls,timers,html:()=>html,version:x=>version=x,status:x=>status=x,upload:x=>upload=x,apply:x=>apply=x};
}
test('upload validates before explicit two-step update; dialog is minimal',async()=>{
 const f=setup();f.open();await tick();
 assert.equal(f.node('[data-update-version]').textContent,'1.1.2');
 assert.match(f.html(),/>上传更新</);assert.match(f.html(),/>上传</);assert.doesNotMatch(f.html(),/选择更新包/);
 assert.equal(f.node('.update-actions').hidden,true);
 const input=f.node('input');input.files=[{name:'update.rvu',size:500}];await input.handlers.change();
 assert.equal(f.calls.length,1);assert.match(f.node('[data-update-status]').textContent,/等待确认/);
 const start=f.node('[data-update-start]');await start.handlers.click();assert.equal(f.calls.length,1);assert.equal(start.textContent,'确认更新');
 await f.node('[data-update-cancel]').handlers.click();assert.equal(start.textContent,'更新');
 await start.handlers.click();await start.handlers.click();assert.equal(f.calls.length,2);assert.equal(f.calls[1].body.ticket,'a'.repeat(32));
 assert.equal(input.disabled,true);assert.equal(f.timers.size,1);
 f.dialog.handlers.close();assert.equal(f.timers.size,0);
 assert.doesNotMatch(source,/window\.confirm|voice-update\.rykvo|admin123/);
});
test('closing aborts upload and does not update detached content',async()=>{
 const f=setup();let resolve,signal;f.upload((file,options)=>{signal=options.signal;return new Promise(r=>resolve=r);});
 f.open();await tick();f.node('input').files=[{name:'update.rvu',size:500}];const pending=f.node('input').handlers.change();
 f.dialog.open=false;f.dialog.handlers.close();assert.equal(signal.aborted,true);
 resolve({state:'ready',ticket:'a'.repeat(32)});await pending;
 assert.notEqual(f.node('[data-update-status]').textContent,'等待确认 · ');
});
test('reopening recovers running status; failed and invalid uploads never apply',async()=>{
 const f=setup();f.status({state:'running'});f.open();await tick();assert.equal(f.node('input').disabled,true);
 f.dialog.handlers.close();f.status({state:'failed',error:'UPDATE_INSTALL_FAILED'});f.open();await tick();assert.match(f.node('[data-update-status]').textContent,/未完成/);
 f.node('input').files=[{name:'wrong.txt',size:3}];await f.node('input').handlers.change();assert.equal(f.calls.length,0);
});
test('XHR upload uses mount, CSRF, progress, timeout and cancellation',async()=>{
 let xhr;
 class XHR {constructor(){xhr=this;this.upload={};this.headers={};} open(method,url){this.method=method;this.url=url;}setRequestHeader(k,v){this.headers[k]=v;}send(file){this.file=file;}abort(){this.onabort();}}
 const ctx=vm.createContext({XMLHttpRequest:XHR,document:{querySelector:()=>({getAttribute:()=>'/gly/'})}});
 vm.runInContext(readFileSync(join(__dirname,'../http.js'),'utf8'),ctx);
 const http=vm.runInContext('Http',ctx);http.setCSRFToken('test-csrf');const progress=[];
 let promise=http.upload('/software-update/upload','blob',{headers:{'Upload-ID':'a'.repeat(32),'Upload-Offset':'0'}});
 assert.equal(xhr.url,'/gly/api/software-update/upload');assert.equal(xhr.timeout,80000);assert.equal(xhr.headers['X-CSRF-Token'],'test-csrf');assert.equal(xhr.headers['Content-Type'],'application/octet-stream');assert.equal(xhr.headers['Upload-Offset'],'0');
 xhr.status=200;xhr.response={data:{state:'ready'}};xhr.onload();assert.equal((await promise).state,'ready');
 promise=http.upload('/software-update/upload','blob');xhr.status=413;xhr.onload();await assert.rejects(promise,{code:'HTTP_413'});
 const ctrl=new AbortController();promise=http.upload('/software-update/upload','blob',{signal:ctrl.signal});ctrl.abort();await assert.rejects(promise,{code:'ABORTED'});
});

async function pollOnce(f){
 const [id,fn]=f.timers.entries().next().value;f.timers.delete(id);await fn();
}
test('upload percentage, validation and failure are real states; repeat submission is blocked',async()=>{
 const f=setup();let finish,options;
 f.upload((file,opts)=>{options=opts;return new Promise(resolve=>finish=resolve);});
 f.open();await tick();const input=f.node('input');input.files=[{name:'update.rvu',size:500}];
 const pending=input.handlers.change();assert.equal(f.node('[data-update-status]').textContent,'正在上传 · 0%');
 await input.handlers.change();assert.equal(f.calls.length,1);
 options.onProgress(35);assert.equal(f.node('progress').value,35);assert.equal(f.node('[data-update-status]').textContent,'正在上传 · 35%');
 options.onProgress(100);assert.match(f.node('[data-update-status]').textContent,/正在校验/);
 finish({state:'ready',version:'1.1.2',ticket:'a'.repeat(32)});await pending;
 assert.equal(f.node('progress').hidden,true);assert.equal(f.node('[data-update-status]').textContent,'等待确认 · 1.1.2');
 const start=f.node('[data-update-start]');await start.handlers.click();await start.handlers.click();await start.handlers.click();
 assert.equal(f.calls.length,2);assert.equal(input.disabled,true);
 f.status(Object.assign(new Error('offline'),{code:'NETWORK'}));await pollOnce(f);
 assert.equal(f.node('[data-update-status]').textContent,'正在重新连接…');assert.equal(f.timers.size,1);
 f.status({state:'failed',error:'UPDATE_INSTALL_FAILED'});await pollOnce(f);
 assert.match(f.node('[data-update-status]').textContent,/未完成/);assert.equal(f.timers.size,0);assert.equal(input.disabled,false);
});
test('already refreshed completion is hidden; outdated page still offers reload',async()=>{
 for(const pageVersion of ['1.1.2','1.1.1','']){
  const f=setup(pageVersion);f.status({state:'complete',version:'1.1.2'});f.open();await tick();
  const fresh=pageVersion==='1.1.2';assert.equal(f.node('[data-update-reload]').hidden,fresh);
  assert.equal(f.node('.update-actions').hidden,fresh);
  assert.equal(f.node('[data-update-status]').textContent,fresh?'':'更新完成 · 1.1.2');assert.equal(f.timers.size,0);
 }
});
test('observed same-version installation keeps completion across modal reopen until page reload',async()=>{
 const f=setup();f.status({state:'running',version:'1.1.2'});f.open();await tick();
 assert.match(f.node('[data-update-status]').textContent,/正在更新，请勿关闭主机/);
 f.status({state:'complete',version:'1.1.2'});await pollOnce(f);
 assert.equal(f.node('[data-update-reload]').hidden,false);assert.equal(f.node('[data-update-status]').textContent,'更新完成 · 1.1.2');
 f.dialog.handlers.close();f.open();await tick();assert.equal(f.node('[data-update-reload]').hidden,false);
 await f.node('[data-update-reload]').handlers.click();assert.equal(f.calls.at(-1),'reload');
});
test('fast completed apply and queued state have explicit status',async()=>{
 const f=setup();f.status({state:'ready',version:'1.1.2',ticket:'a'.repeat(32)});
 f.apply(async()=>({state:'complete',version:'1.1.2'}));f.open();await tick();
 const start=f.node('[data-update-start]');await start.handlers.click();await start.handlers.click();
 assert.equal(f.node('[data-update-reload]').hidden,false);assert.equal(f.node('[data-update-status]').textContent,'更新完成 · 1.1.2');
 const queued=setup();queued.status({state:'queued',version:'1.1.2'});queued.open();await tick();
 assert.equal(queued.node('input').disabled,true);assert.match(queued.node('[data-update-status]').textContent,/等待更新/);
 queued.dialog.handlers.close();
});
test('late version failure never hides upload status and invalid selections reset input',async()=>{
 const f=setup();let reject;f.version(()=>new Promise((resolve,r)=>reject=r));f.open();await tick();
 f.node('input').files=[{name:'update.rvu',size:500}];await f.node('input').handlers.change();
 reject({code:'NETWORK'});await tick();assert.equal(f.node('[data-update-version]').textContent,'读取失败');
 assert.equal(f.node('[data-update-status]').textContent,'等待确认 · 1.1.2');
 f.node('input').files=[{name:'bad.txt',size:5}];f.node('input').value='bad.txt';await f.node('input').handlers.change();
 assert.equal(f.node('input').value,'');assert.equal(f.node('[data-update-file]').textContent,'');
 assert.equal(f.node('[data-update-start]').hidden,true);assert.equal(f.calls.length,1);
});
test('completion clears the package and displays the new version even after a stale version response',async()=>{
 const f=setup();let version;
 f.version(()=>new Promise(resolve=>version=resolve));f.open();await tick();
 f.node('input').files=[{name:'RykvoVoice 1.1.3.rvu',size:500}];await f.node('input').handlers.change();
 assert.match(f.node('[data-update-file]').textContent,/1.1.3/);
 const start=f.node('[data-update-start]');await start.handlers.click();await start.handlers.click();
 f.status({state:'complete',version:'1.1.3'});await pollOnce(f);
 assert.equal(f.node('[data-update-file]').textContent,'');assert.equal(f.node('[data-update-version]').textContent,'1.1.3');
 version({version:'1.1.2'});await tick();assert.equal(f.node('[data-update-version]').textContent,'1.1.3');
 assert.match(f.html(),/class="text-button update-cancel"/);assert.doesNotMatch(f.html(),/class="btn/);
});
test('old historical completion never replaces the actual current version or prompts another reload',async()=>{
 const f=setup('1.1.3');f.version(async()=>({version:'1.1.3'}));f.status({state:'complete',version:'1.1.2'});
 f.open();await tick();assert.equal(f.node('[data-update-version]').textContent,'1.1.3');
 assert.equal(f.node('[data-update-status]').textContent,'');assert.equal(f.node('.update-actions').hidden,true);
});
