const { test } = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const { readFileSync } = require('node:fs');
const { join } = require('node:path');
function setup() {
  const events = {}, dialogs = [], calls = [], notices = [];
  const data = { current: [{ cid: 2, apn: 'ims', protocol: 'IPV4V6' }], profiles: [{ id: 'profile-000000001', apn: '<unsafe>', protocol: 'IP', auth: 'PAP', username: 'user', hasPassword: true }], issue: '' };
  let reply = async () => data;
  const context = vm.createContext({ AbortController,
    document: { addEventListener: (k,f) => { events[k] = f; }, getElementById: () => ({ addEventListener: (k,f) => { events[k] = f; } }), querySelectorAll: () => [] },
    UI: { modal: (...v) => dialogs.push(v), escape: v => String(v).replaceAll('<','&lt;').replaceAll('"','&quot;'), toast: v => notices.push(v) },
    Http: { id: () => 'apn-generated-test-id' }, Forms: { field: () => '<input>' },
    ModuleData: { issueText: c => c },
    Backend: { modules: Object.fromEntries(['apns','saveAPN','applyAPN','removeAPN'].map(op => [op, async opts => { calls.push({ op, opts }); return reply(op,opts); }])) },
    localStorage: { setItem: () => { throw Error('APN credentials must not be stored in browser'); } },
  });
  vm.runInContext(readFileSync(join(__dirname,'..','cellular-apn.js'),'utf8'),context);
  const api = vm.runInContext('CellularAPN',context);
  const click = (selector,dataset={}) => events.click({ target: { closest: s => s === '#dialog-content' ? {} : s === selector ? { dataset } : null } });
  return { api, events, dialogs, calls, notices, click, setReply: fn => { reply=fn; } };
}
const tick = async () => { for (let i=0;i<6;i++) await Promise.resolve(); };
test('APN loads server data, escapes fields and returns to selected line', async () => {
  const f=setup();let back=0;
  f.api.open({id:'module-03',managed:true},{id:'line-uk',label:'英国'},()=>back++);await tick();
  assert.equal(f.dialogs.at(-1)[0],'蜂窝数据网络');
  const html=f.dialogs.at(-1)[1];assert.match(html,/ims/);assert.match(html,/&lt;unsafe>/);assert.doesNotMatch(html,/<unsafe>|使用中/);
  assert.doesNotMatch(html,/配置按当前 SIM 保存|保存不会启用流量；应用只写入/);
  assert.equal(f.calls[0].opts.params.lineId,'line-uk');
  f.click('[data-apn-back]',{apnBack:'line'});assert.equal(back,1);assert.equal(f.calls[0].opts.signal.aborted,true);
});
test('late APN response cannot overwrite a closed dialog',async()=>{
  const f=setup();let resolve;f.setReply(()=>new Promise(r=>{resolve=r}));
  f.api.open({id:'module-01',managed:true},{id:'line-a'},()=>{});const n=f.dialogs.length;
  f.events.close();resolve({profiles:[],current:[{cid:1,apn:'old-card'}]});await tick();assert.equal(f.dialogs.length,n);
});
test('save preserves masked password explicitly, deduplicates submits and does not apply',async()=>{
  const f=setup();f.api.open({id:'module-03',managed:true},{id:'line-uk'},()=>{});await tick();
  f.click('[data-apn-edit]',{apnEdit:'profile-000000001'});
  const values={apn:' internet ',protocol:'IPV4V6',auth:'PAP',username:'user',password:''};
  const event={preventDefault(){},target:{id:'cellular-apn-form',elements:{namedItem:k=>({value:values[k]})}}};
  f.events.submit(event);f.events.submit(event);await tick();
  const saved=f.calls.filter(c=>c.op==='saveAPN');assert.equal(saved.length,1);assert.equal(saved[0].opts.body.apn,'internet');assert.equal(saved[0].opts.body.preservePassword,true);assert.equal(saved[0].opts.params.lineId,'line-uk');
  assert.equal(f.calls.filter(c=>c.op==='applyAPN').length,0);
});
test('applying APN requires confirmation separate from save',async()=>{
  const f=setup();f.api.open({id:'module-03',managed:true},{id:'line-uk'},()=>{});await tick();
  f.click('[data-apn-edit]',{apnEdit:'profile-000000001'});f.click('[data-apn-apply]');
  assert.equal(f.calls.filter(c=>c.op==='applyAPN').length,0);assert.match(f.dialogs.at(-1)[1],/不开启流量/);
  f.click('[data-apn-confirm]',{apnConfirm:'apply'});await tick();assert.equal(f.calls.filter(c=>c.op==='applyAPN').length,1);
  assert.ok(f.notices.includes('APN 已写入，未启用流量'));
});
