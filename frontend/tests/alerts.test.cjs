const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup() {
  const context = vm.createContext({ document: { addEventListener() {} }, UI: { escape: value => String(value).replaceAll("<", "&lt;").replaceAll('"', "&quot;") }, ModuleData: { items: [], labels: {} } });
  for (const file of ["alerts.js", "modules.js", "developer.js"]) vm.runInContext(readFileSync(join(__dirname, "..", file), "utf8"), context);
  return vm.runInContext("({Alerts,Modules,Developer})", context);
}
test("SIM filter counts modules once and remains independent from physical status", () => {
  const { Modules, Alerts } = setup();
  const items = [{name:"模块 01",status:"online",alerts:[{kind:"sms",active:true},{kind:"mms",active:true}]},{name:"模块 02",status:"error",alerts:[{kind:"module",active:true}]},{name:"模块 03",status:"offline",alerts:[{kind:"sip",active:true}]},{name:"模块 04",status:"online",alerts:[{kind:"sms",active:false}]}];
  assert.deepEqual(JSON.parse(JSON.stringify(Modules.count(items))), { all:4,online:2,offline:1,error:1,sim:2 });
  assert.equal(Modules.select(items,"sim").length,2);
  assert.equal(Alerts.sim(items[1]),false);
  assert.equal(Alerts.badge(items[3]),"");
});
test("alerts show only reached thresholds, escape descriptions and enforce whole limits", () => {
  const { Alerts } = setup();
  assert.equal(Alerts.details({alerts:[{active:false,kind:"sms",failures:4}]}),"");
  const html=Alerts.details({alerts:[{active:true,kind:"sms",failures:5,description:"<untrusted>"}]});
  assert.match(html,/连续失败 5 次/);assert.doesNotMatch(html,/<untrusted>/);
  const moduleHTML=Alerts.details({alerts:[{active:true,kind:"module",failures:5,description:"Wi-Fi 通话未注册"}]});
  assert.match(moduleHTML,/连续检测 5 次/);assert.doesNotMatch(moduleHTML,/重连.*5/);
  for (const value of ["101","1.5","-2","", "5x"]) assert.ok(Alerts.validate({sip:value,message:5,module:5}));
  assert.equal(Alerts.validate({sip:"0",message:"0",module:"0"}),null);
  assert.equal(Alerts.validate({sip:"5",message:"3",module:"100"}),null);
});
test("developer settings preserve blank secrets and send only explicit clears", () => {
  const { Developer } = setup();
  const form={dataset:{revision:"4",apiRevision:"2"},elements:{namedItem:name=>({dataset:name==="telegramProxy"?{clear:"true"}:{}})}};
  const body=Developer.payload({botToken:"",telegramProxy:"",adminId:"12",notificationId:"-10042",apiKey:"",webhook:"https://example.test/hook",unknown:"discard"},form);
  assert.deepEqual(JSON.parse(JSON.stringify(body)),{revision:4,apiRevision:2,webhook:"https://example.test/hook",adminId:"12",notificationId:"-10042",telegramProxy:null});
});
