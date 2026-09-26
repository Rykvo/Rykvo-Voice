const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const {readFileSync}=require('node:fs');
const {join}=require('node:path');
function setup(operation=async()=>({state:'configuring'})) {
 const fields={address:{value:''},accessCode:{value:''}},button={classList:{toggle(){}}},note={dataset:{}};
 const events={},calls=[],reports=[],timers=new Map();let sequence=0;
 let snapshot={state:'disconnected',configured:false,enabled:false};
 const form={elements:{namedItem:k=>fields[k]},querySelector:s=>s.includes('submit')?button:note,addEventListener:(k,f)=>events[k]=f,removeEventListener:k=>delete events[k],reset(){fields.address.value='';fields.accessCode.value=''}};
 const call=name=>async options=>{calls.push({name,...options});return operation(options)};
 const context=vm.createContext({URL,AbortController,setTimeout:f=>{timers.set(++sequence,f);return sequence},clearTimeout:i=>timers.delete(i),ServerNavigation:{header:()=>"SIP 电话服务器"},document:{getElementById:()=>form},Forms:{field:x=>JSON.stringify(x),report:(_,e)=>{if(e)reports.push(e);return !!e}},Backend:{enabled:()=>true,sipServer:{get:async()=>{await Promise.resolve();return snapshot},connect:call('connect'),logout:call('logout')}},localStorage:{setItem(){throw Error('credentials persisted')}}});
 vm.runInContext(readFileSync(join(__dirname,'../sip-server.js'),'utf8'),context);
 const page=vm.runInContext('SIPServer',context);page.mount();
 return {page,fields,button,note,calls,reports,timers,setStatus:x=>snapshot=x,ready:()=>new Promise(r=>setImmediate(r)),tick:async()=>{const [id,fn]=timers.entries().next().value||[];if(fn){timers.delete(id);await fn()}},submit:()=>events.submit({type:'submit',preventDefault(){}})};
}
test('single HTTPS form and backend-verified network status',async()=>{
 const f=setup();await f.ready();const html=f.page.render();assert.ok(html.includes("https://sip.example.com/api/connect"));assert.match(html,/"type":"password"/);
 f.fields.address.value='sip.example.com/api/connect';f.fields.accessCode.value='A'.repeat(43);await f.submit();
 assert.equal(f.calls[0].body.address,'https://sip.example.com/api/connect');assert.equal(f.fields.address.value,'https://sip.example.com/api/connect');
 assert.equal(f.calls.length,1);assert.equal(f.fields.accessCode.value,'');assert.equal(f.note.textContent,'配置中');assert.doesNotMatch(f.note.textContent,/已连接/);
 f.setStatus({state:'connected',configured:true,enabled:true,address:f.fields.address.value});await f.tick();assert.equal(f.note.textContent,'服务器已连接');assert.equal(f.note.dataset.failed,'false');assert.equal(f.fields.address.readOnly,true);f.page.unmount();assert.equal(f.timers.size,0);
});
test('invalid URL or code does not submit',async()=>{
 const f=setup();await f.ready();f.fields.address.value='sip.example.com:5061';await f.submit();assert.equal(f.reports.at(-1).field,'address');
 f.fields.address.value='https://sip.example.com/api/connect';await f.submit();assert.equal(f.reports.at(-1).field,'accessCode');assert.equal(f.calls.length,0);f.page.unmount();
});
test('logout clears credentials and allows switching to another SIP server',async()=>{
 const f=setup();f.setStatus({state:'connected',configured:true,enabled:true,address:'https://sip.example.com/api/connect'});await f.ready();
 assert.equal(f.button.textContent,'注销');assert.equal(f.fields.address.readOnly,true);
 await f.submit();assert.equal(f.calls[0].name,'logout');assert.deepEqual(JSON.parse(JSON.stringify(f.calls[0].body)),{});assert.equal(f.fields.address.value,'');
 f.setStatus({state:'disconnected',configured:false,enabled:false,address:''});await f.tick();
 assert.equal(f.button.textContent,'连接');assert.equal(f.fields.address.readOnly,false);assert.equal(f.fields.accessCode.disabled,false);
 f.fields.address.value='other.example.com/api/connect';f.fields.accessCode.value='B'.repeat(43);await f.submit();
 assert.equal(f.calls[1].name,'connect');assert.equal(f.calls[1].body.address,'https://other.example.com/api/connect');f.page.unmount();
});
test('duplicate submit blocked; unmount aborts and clears credentials and timers',async()=>{
 let resolve;const f=setup(()=>new Promise(r=>resolve=r));await f.ready();f.fields.address.value='https://sip.example.com/api/connect';f.fields.accessCode.value='A'.repeat(43);
 const pending=f.submit();await f.submit();assert.equal(f.calls.length,1);f.page.unmount();assert.equal(f.calls[0].signal.aborted,true);resolve({ok:true});await pending;assert.equal(f.fields.accessCode.value,'');assert.equal(f.timers.size,0);
});
test('failed handshake never reports a working telephone',async()=>{
 const f=setup();f.setStatus({state:'failed',configured:true,enabled:true,issue:'SIP_HANDSHAKE_TIMEOUT'});await f.ready();assert.equal(f.note.textContent,'VPN 握手超时');assert.doesNotMatch(f.note.textContent,/电话已连接|已送达/);f.page.unmount();
});
