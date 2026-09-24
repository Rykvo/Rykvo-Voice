const { test } = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
function setup(connect = async () => { throw Object.assign(Error('offline'), {code:'NOT_CONNECTED'}); }) {
  const fields = {address:{value:'sip.example.com:5061'}, accessCode:{value:'fixture-code'}};
  const button = {disabled:false,textContent:'连接'}, note = {hidden:true,textContent:''};
  const events = {}, calls = [], reports = [];
  const form = {
    elements:{namedItem:name=>fields[name]},
    querySelector:s=>s.includes('submit')?button:note,
    addEventListener:(k,fn)=>events[k]=fn,
    removeEventListener:k=>delete events[k],
    reset(){ for(const field of Object.values(fields)) field.value=''; }
  };
  const context=vm.createContext({
    AbortController,
    document:{getElementById:()=>form},
    Forms:{header:t=>`<h1>${t}</h1>`,field:options=>JSON.stringify(options),report:(_form,error)=>{if(error)reports.push(error);return !!error;}},
    Backend:{sipServer:{connect:options=>{calls.push(options);return connect(options);}}},
    localStorage:{setItem(){throw Error('must not persist credentials');}},
  });
  vm.runInContext(readFileSync(join(__dirname,'..','sip-server.js'),'utf8'),context);
  const page=vm.runInContext('SIPServer',context);page.mount();
  return {page,fields,button,note,calls,reports,submit:()=>events.submit({preventDefault(){}})};
}
test('SIP server renders address, masked access code and connection action',()=>{
  const f=setup(),html=f.page.render();
  for(const s of ['SIP 电话服务器','接入地址','接入码','"type":"password"','>连接<'])assert.ok(html.includes(s));
  assert.doesNotMatch(html,/已连接|连接成功/);
});
test('reserved submit sends contract once, clears secret and displays unavailable result',async()=>{
  const f=setup();await f.submit();
  assert.equal(f.calls.length,1);assert.equal(f.calls[0].body.accessCode,'fixture-code');
  assert.equal(f.fields.accessCode.value,'');assert.equal(f.note.textContent,'SIP 电话服务尚未接入');
  assert.equal(f.note.hidden,false);assert.equal(f.button.disabled,false);
});
test('missing address and access code do not submit',async()=>{
  const f=setup();f.fields.address.value='';await f.submit();assert.equal(f.reports.at(-1).field,'address');
  f.fields.address.value='sip.example.com';f.fields.accessCode.value='';await f.submit();assert.equal(f.reports.at(-1).field,'accessCode');assert.equal(f.calls.length,0);
});
test('duplicate submission is blocked and navigating away cancels and clears fields',async()=>{
  let resolve;const f=setup(()=>new Promise(r=>resolve=r));
  const pending=f.submit();await f.submit();assert.equal(f.calls.length,1);assert.equal(f.button.disabled,true);
  f.page.unmount();assert.equal(f.calls[0].signal.aborted,true);assert.equal(f.fields.accessCode.value,'');
  resolve({status:'connected'});await pending;assert.equal(f.note.hidden,true);
});
test('reserved UI never treats an arbitrary successful response as a verified connection',async()=>{
  const f=setup(async()=>({ok:true}));await f.submit();assert.doesNotMatch(f.note.textContent,/成功|已连接/);
});
