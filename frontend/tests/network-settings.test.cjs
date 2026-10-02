const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const ctx = vm.createContext({UI:{escape:s=>String(s).replaceAll('&','&amp;').replaceAll('<','&lt;').replaceAll('"','&quot;')},Forms:{header:s=>s}});
vm.runInContext(fs.readFileSync(__dirname+'/../network-settings.js','utf8'),ctx);
const api = vm.runInContext('NetworkSettings',ctx), id='a'.repeat(32);
const n = {id,label:'LAN',name:'eth0',state:'configured',hostDefault:['IPv4'],addresses:[{family:'inet',scope:'global',local:'192.0.2.1'}]};
test('four columns with actual network status below name',()=>{
 const html=api.table({networks:[n],modules:[]});
 assert.match(html,/<th>网络<\/th><th>主网络<\/th><th>VPN<\/th><th>操作<\/th>/);
 assert.match(html,/已连接 · 192.0.2.1/); assert.doesNotMatch(html,/<th>状态/);
 assert.match(html,/checked[^>]+disabled/);
});
test('unused adapters hidden, assigned or configured offline links retained',()=>{
 const net={...n,state:'no-carrier',hostDefault:[],addresses:[]};
 assert.equal(api.visibleNetwork(net,[]),false);
 assert.equal(api.visibleNetwork(net,[{network:id}]),true);
 assert.equal(api.visibleNetwork({...net,vpn:{configured:true}},[]),true);
});
test('test result is never presented as an active tunnel',()=>{
 const net={...n,vpn:{configured:true,testState:'passed',testExitIP:'203.0.113.5',label:'<img>',security:'none'}};
 assert.match(api.vpnStatus(net),/未接管流量/);
 const html=api.detailRows(net);
 assert.match(html,/检测出口 IP/); assert.match(html,/&lt;img>/); assert.doesNotMatch(html,/<img>/);
 assert.match(html,/未启用 TLS/); assert.match(html,/<textarea name="vpnLink"/);
 assert.doesNotMatch(html,/type="password"/);
 assert.ok(html.indexOf('局域网 IP')<html.indexOf('class="network-vpn"'));
 assert.ok(html.indexOf('class="network-vpn"')<html.indexOf('诊断信息'));
 assert.doesNotMatch(html,/role="switch"/);
});
test('IPv6 stays in diagnostics when LAN IPv4 is present',()=>{
 const html=api.detailRows({...n,addresses:[...n.addresses,{family:'inet6',local:'fd00::1'}]});
 assert.ok(html.indexOf('fd00::1')>html.indexOf('诊断信息'));
 assert.match(html,/192.0.2.1/);
});
test('only blank backdrop is outside, not interior space or controls',()=>{
 const dialog={getBoundingClientRect:()=>({left:100,right:500,top:100,bottom:700})};
 const event={target:dialog,clientX:80,clientY:400};
 assert.equal(api.outsideDialog(event,dialog),true);
 assert.equal(api.outsideDialog({...event,clientX:120},dialog),false);
 assert.equal(api.outsideDialog({...event,target:{}},dialog),false);
 assert.equal(api.outsideDialog({...event,clientX:300,clientY:800},dialog),true);
});
test('direct switching preserves other modules and busy protection',()=>{
 const net={...n,hostDefault:[]};
 const modules=[{id:'module-01',network:'b'.repeat(32),busy:false},{id:'module-02',network:id,busy:true}];
 assert.deepEqual(Array.from(api.assignmentUpdate(modules,net,'module-01',true)),['module-01','module-02']);
 assert.equal(api.assignmentUpdate(modules,net,'module-02',false),null);
 assert.equal(modules[0].network,'b'.repeat(32));
 assert.equal(api.assignmentUpdate(modules,{...net,state:'no-carrier'},'module-01',true),null);
 assert.equal(api.assignmentUpdate(modules,net,'missing',true),null);
});
test('only the assigned network is checked and other connected networks remain clickable',()=>{
 const b={...n,id:'b'.repeat(32),label:'USB'};
 const module={id:'module-01',label:'01',network:id,busy:false};
 const selected=()=>api.assignmentRows([module],n,[n,b]);
 const other=()=>api.assignmentRows([module],b,[n,b]);
 assert.match(selected(),/checked/);assert.doesNotMatch(selected(),/disabled/);
 assert.doesNotMatch(other(),/checked|disabled/);
 module.network=b.id;
 assert.doesNotMatch(selected(),/checked|disabled/);assert.match(other(),/checked/);
 assert.deepEqual(Array.from(api.assignmentUpdate([module],b,module.id,false)),[]);
 module.network='';
 assert.doesNotMatch(selected()+other(),/checked|disabled/);
 assert.match(other(),/主机网络/);
});
test('offline assigned networks can be cleared but never selected as new targets',()=>{
 const offline={...n,state:'unavailable'};
 const module={id:'module-01',label:'01',network:id,busy:false};
 assert.deepEqual(Array.from(api.assignmentUpdate([module],offline,module.id,false)),[]);
 assert.doesNotMatch(api.assignmentRows([module],offline,[offline]),/disabled/);
 module.network='';
 assert.equal(api.assignmentUpdate([module],offline,module.id,true),null);
 assert.match(api.assignmentRows([module],offline,[offline]),/disabled/);
});
test('unchanged polling text does not replace DOM nodes',()=>{
 let value='未开启',writes=0;
 const element={get textContent(){return value;},set textContent(next){value=next;writes++;}};
 api.setText(element,'未开启');
 assert.equal(writes,0);
 api.setText(element,'正在检测');
 assert.equal(writes,1);
 api.setText(element,'正在检测');
 assert.equal(writes,1);
 api.setText(null,'');
});
test('details have one scrolling region and no textarea resize handle',()=>{
 const css=fs.readFileSync(__dirname+'/../network-settings.css','utf8');
 assert.match(css,/\.network-vpn-field textarea\s*\{[^}]*resize: none;[^}]*height: 88px;/);
 assert.match(css,/#dialog:has\(#network-edit\)\s*\{[^}]*overflow: clip;[^}]*animation: none;/);
 assert.match(css,/#network-edit\[data-details\]\s*\{[^}]*overflow-y: auto;/);
 assert.match(api.detailRows(n),/data-network-name-row/);
});
test('node retry action never matches the form request counter',()=>{
 const source=fs.readFileSync(__dirname+'/../network-settings.js','utf8');
 assert.match(source,/dataset\.vpnRequest/);
 assert.match(source,/closest\("button\[data-vpn-retry\]"\)/);
 assert.doesNotMatch(source,/data-vpn-load|dataset\.vpnLoad/);
 assert.match(api.detailRows(n),/<button[^>]+data-vpn-retry/);
});
test('list actions and column wrappers retain the original bindings',()=>{
 const html=api.table({networks:[n,{...n,id:'b'.repeat(32),hostDefault:[],label:'<Network>'}],modules:[]});
 assert.equal((html.match(/class="network-control"/g)||[]).length,4);
 assert.equal((html.match(/data-network-detail=/g)||[]).length,2);
 assert.equal((html.match(/data-network-assign=/g)||[]).length,2);
 assert.match(html,/class="network-actions"/);
 assert.match(html,/&lt;Network>/);
 assert.doesNotMatch(html,/<Network>/);
});
test('mobile cards reuse controls with separate main and VPN panels',()=>{
 const html=api.table({networks:[n],modules:[]});
 assert.equal((html.match(/role="switch"/g)||[]).length,2);
 assert.match(html,/class="network-main-cell"><span class="network-mobile-label" aria-hidden="true">主网络/);
 assert.match(html,/class="network-vpn-cell"><span class="network-mobile-label" aria-hidden="true">VPN/);
 assert.equal((html.match(/data-network-detail=/g)||[]).length,1);
 const css=fs.readFileSync(__dirname+'/../network-settings.css','utf8');
 assert.match(css,/@container network-layout \(max-width: 720px\)/);
 assert.match(css,/grid-template-columns: repeat\(2, minmax\(0, 1fr\)\)/);
 assert.match(css,/tbody th:first-child\s*\{[^}]*width: auto;/);
 assert.match(css,/\.network-actions-cell\s*\{[^}]*grid-column: 1 \/ -1;/);
 assert.match(css,/\.text-button\s*\{[^}]*flex: 1;[^}]*min-height: 44px;/);
 assert.match(css,/\.form-switch input\s*\{[^}]*height: 44px;/);
});
test('mobile details avoid focus zoom and retain fixed textarea sizing',()=>{
 const css=fs.readFileSync(__dirname+'/../network-settings.css','utf8');
 assert.match(css,/\.network-rename input\s*\{ font-size: 16px;/);
 assert.match(css,/\.dialog-close\s*\{[^}]*width: 44px; height: 44px;/);
 assert.match(css,/\.network-vpn-field textarea\s*\{ height: 104px;/);
});
test('interface has no development copy or unused preview styles',()=>{
 const html=api.render()+api.table({networks:[n],modules:[]})+api.detailRows(n);
 assert.doesNotMatch(html,/预览|测试|待联调|待启用/);
 assert.doesNotMatch(html,/主网络切换暂不可用|VPN 尚未接管流量/);
 assert.match(html,/data-vpn-action="test">检测连接/);
 const css=fs.readFileSync(__dirname+'/../network-settings.css','utf8');
 assert.doesNotMatch(css,/network-control-note/);
});

test('live switches reflect backend readiness and actual routing state',()=>{
 const html=api.table({networks:[{...n,primary:false,routingAvailable:true,vpn:{configured:true,enabled:true,state:'active'}}],modules:[]});
 assert.doesNotMatch(html,/disabled/);
 assert.match(html,/data-network-toggle="primary"/);
 assert.match(html,/data-network-toggle="vpn"[^>]+checked/);
 assert.match(html,/已开启/);
 assert.match(api.vpnStatus({...n,vpn:{enabled:true,state:'blocked'}}),/已阻止直连/);
 assert.doesNotMatch(api.detailRows(n),/恢复默认分配/);
});

test('a recovered read unlocks switches even when cached markup is unchanged',async()=>{
 let poll,fail=false;
 const input={dataset:{network:id,networkToggle:'primary'},disabled:false};
 const list={innerHTML:''},error={textContent:''},dialog={addEventListener(){}};
 const data={revision:1,networks:[{...n,primary:true,routingAvailable:true}],modules:[]};
 const context=vm.createContext({
  AbortController,clearInterval(){},setInterval(fn){poll=fn;return 1;},
  document:{hidden:false,addEventListener(){},querySelectorAll(){return [input];}},
  Forms:{header:s=>s},
  UI:{escape:String,$:s=>({'#network-list':list,'#network-error':error,'#dialog':dialog}[s]||null)},
  Http:{async request(){if(fail)throw new Error('temporary');return data;}}
 });
 vm.runInContext(fs.readFileSync(__dirname+'/../network-settings.js','utf8'),context);
 const ui=vm.runInContext('NetworkSettings',context);
 await ui.mount();assert.equal(input.disabled,false);
 fail=true;await poll();assert.equal(input.disabled,true);
 fail=false;await poll();assert.equal(input.disabled,false);
 assert.equal(error.textContent,'');ui.unmount();
});

test('switch saves once, waits for confirmation and reconciles failure without resending',async()=>{
 const b='b'.repeat(32),listeners={},notices=[],writes=[];
 const data={revision:1,networks:[n,{...n,id:b,label:'USB'}],modules:[{id:'module-01',label:'01',number:'',network:id,busy:false}]};
 const list={innerHTML:''},dialog={addEventListener(){},close(){}},error={textContent:''};
 let form=null,resolveWrite,rejectWrite;
 const context=vm.createContext({
  AbortController,clearInterval(){},setInterval(){return 1;},
  document:{hidden:false,activeElement:null,addEventListener(name,fn){listeners[name]=fn;},querySelectorAll(){return [];}},
  Forms:{header:s=>s},
  UI:{escape:String,toast:s=>notices.push(s),
   $:s=>({'#network-list':list,'#network-error':error,'#dialog':dialog,'#network-edit':form}[s]||null),
   modal(title,html){form={dataset:{network:html.match(/data-network="([a-f0-9]+)"/)[1]},innerHTML:'',events:{},attrs:{},
    contains(){return false;},querySelector(){return null;},querySelectorAll(){return [];},remove(){form=null;},
    getAttribute(k){return this.attrs[k];},setAttribute(k,v){this.attrs[k]=v;},addEventListener(k,fn){this.events[k]=fn;}};}
  },
  Http:{async request(){return structuredClone(data);}},
  Backend:{networks:{update({body}){writes.push(JSON.parse(JSON.stringify(body)));return new Promise((resolve,reject)=>{resolveWrite=resolve;rejectWrite=reject;});}}}
 });
 vm.runInContext(fs.readFileSync(__dirname+'/../network-settings.js','utf8'),context);
 const ui=vm.runInContext('NetworkSettings',context),tick=()=>new Promise(resolve=>setImmediate(resolve));
 const open=network=>listeners.click({target:{closest:s=>s==='[data-network-assign]'?{dataset:{networkAssign:network}}:null}});
 const toggle=enabled=>form.events.change({target:{closest:()=>({dataset:{module:'module-01'},checked:enabled})}});
 await ui.mount();open(b);
 assert.doesNotMatch(form.innerHTML,/checked|disabled/);
 toggle(true);toggle(true);
 assert.equal(writes.length,1);
 assert.deepEqual(writes[0],{id:b,revision:1,modules:['module-01']});
 assert.equal(data.modules[0].network,id);
 data.modules[0].network=b;data.revision=2;resolveWrite();await tick();
 assert.match(form.innerHTML,/checked/);
 open(id);assert.doesNotMatch(form.innerHTML,/checked|disabled/);
 open(b);toggle(false);rejectWrite({code:'NETWORK_CONFLICT'});await tick();
 assert.equal(writes.length,2);assert.match(form.innerHTML,/checked/);
 assert.equal(notices.length,1);
 toggle(false);data.modules[0].network='';data.revision=3;resolveWrite();await tick();
 assert.equal(writes.length,3);assert.doesNotMatch(form.innerHTML,/checked|disabled/);
 assert.match(form.innerHTML,/主机网络/);
 ui.unmount();
});
