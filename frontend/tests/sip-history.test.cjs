const { test } = require("node:test");
const assert = require("node:assert/strict");
const vm = require("node:vm");
const { readFileSync } = require("node:fs");
const { join } = require("node:path");
function setup() {
  const nodes = new Map(), events = {}, requests = [];
  let respond = async () => ({ items: [], nextCursor: "", totalMinutes:0,outgoingMinutes:0,incomingMinutes:0 });
  const node = key => { if (!nodes.has(key)) nodes.set(key,{ innerHTML:"", textContent:"", disabled:false, hidden:false }); return nodes.get(key); };
  const context = vm.createContext({
    AbortController,
    UI: {escape: s=>String(s).replaceAll("&","&amp;").replaceAll("<","&lt;").replaceAll('"',"&quot;"), $:node,read:()=>{throw Error("legacy local records read");}},
    Calendar: null, Countries:{format:String}, ModuleData:{items:[{id:"module-02",name:"模块 02"}]},
    Backend:{callRecords:{list(o){requests.push(o);return respond(o);}}},
    document:{hidden:false,addEventListener:(t,f)=>(events[t]||=[]).push(f)},
    window:{addEventListener(){}},
  });
  for (const f of ["calendar.js","sip-history.js"]) vm.runInContext(readFileSync(join(__dirname,"..",f),"utf8"),context);
  return {h:vm.runInContext("SIPHistory",context),node,requests,reply(fn){respond=fn;},
    emit(t,target){return Promise.all((events[t]||[]).map(f=>f({target})));}};
}
const base = {id:"1",sipAccountId:"account",number:"10001",moduleId:"module-02",direction:"outgoing",status:"connected",startedAt:"2026-09-27T01:00:00Z",answeredAt:"2026-09-27T01:00:10Z",duration:61};
test("existing four-column view loads real account and local-day records only", async()=>{
  const a=setup();a.reply(async()=>({items:[base,{...base,id:"2",direction:"incoming"}],nextCursor:"",totalMinutes:4,outgoingMinutes:2,incomingMinutes:2}));
  const html=a.h.render("account");
  assert.deepEqual([...html.matchAll(/<th scope="col">([^<]+)<\/th>/g)].map(x=>x[1]),["对方号码","模块","状态","时长"]);
  assert.match(html,/呼出时长/);assert.match(html,/呼入时长/);assert.doesNotMatch(html,/通话统计|sip-history-duration/);
  assert.match(html,/加载中/);await a.h.update();
  const body=a.node("#sip-history-rows").innerHTML;
  assert.equal((body.match(/10001/g)||[]).length,2);assert.match(body,/呼入/);assert.match(body,/呼出/);assert.match(body,/2分钟/);
  assert.equal(a.requests[0].query.accountId,"account");assert.ok(a.requests[0].query.from.endsWith("Z"));
  assert.equal(a.node("#sip-history-outgoing").textContent,"0小时2分");
  assert.equal(a.node("#sip-history-incoming").textContent,"0小时2分");
  assert.match(body,/已接听/);assert.match(body,/已接通/);assert.equal(a.requests.length,1);
});
test("all exact status reasons stay distinct and unknown codes do not invent card faults",()=>{
  const {h}=setup();
  for(const [status,want] of Object.entries({call_not_accepted:"呼叫未接受",module_disconnected:"设备断开",remote_cancelled:"对方挂断",connected:"已接通",no_answer:"无人接听",rejected:"对方拒接",busy:"对方占线",module_busy:"模块忙碌",account_busy:"账号忙碌",call_ending:"通话结束中",card_error:"卡异常",not_registered:"网络未注册",module_error:"模块异常",cancelled:"已取消",unconnected:"未接通",madeup:"拨打失败"})) assert.equal(h.status({status}),want);
  assert.equal(h.minutes({...base,duration:0}),1);assert.equal(h.minutes({...base,duration:60}),1);assert.equal(h.minutes({...base,duration:60.1}),2);
  assert.equal(h.minutes({...base,answeredAt:null}),null);assert.equal(h.minutes({...base,duration:null}),null);
  assert.equal(h.totalDuration(null),"—");assert.equal(h.totalDuration(6000),"100小时0分");
});
test("stale account/date responses and a closed dialog cannot overwrite current data",async()=>{
  const a=setup();let resolve;
  a.reply(()=>new Promise(r=>resolve=r));a.h.render("old");const old=a.h.update();
  a.h.render("new");a.reply(async()=>({items:[{...base,number:"222"}],nextCursor:"",totalMinutes:2,outgoingMinutes:2,incomingMinutes:0}));await a.h.update();
  resolve({items:[{...base,number:"111"}],nextCursor:"",totalMinutes:9,outgoingMinutes:9,incomingMinutes:0});await old;
  assert.match(a.node("#sip-history-rows").innerHTML,/222/);assert.doesNotMatch(a.node("#sip-history-rows").innerHTML,/111/);
  a.reply(()=>new Promise(r=>resolve=r));const later=a.h.update();a.h.close();assert.equal(a.requests.at(-1).signal.aborted,true);
  resolve({items:[],nextCursor:"",totalMinutes:0,outgoingMinutes:0,incomingMinutes:0});await later;assert.match(a.node("#sip-history-rows").innerHTML,/222/);
});
test("pagination is bounded and totals include offscreen records",async()=>{
  const a=setup();a.h.render("account");a.reply(async o=>o.query.before?{items:[{...base,number:"last"}],nextCursor:"",totalMinutes:202,outgoingMinutes:200,incomingMinutes:2}:{items:[base],nextCursor:"cursor",totalMinutes:202,outgoingMinutes:200,incomingMinutes:2});
  await a.h.update();assert.equal(a.node("[data-sip-history-next]").hidden,false);
  await a.emit("click",{closest:s=>s==="[data-sip-history-next]"});await new Promise(setImmediate);
  assert.equal(a.requests.at(-1).query.before,"cursor");assert.match(a.node("#sip-history-rows").innerHTML,/last/);assert.equal(a.node("#sip-history-outgoing").textContent,"3小时20分");
  assert.equal(a.node("#sip-history-incoming").textContent,"0小时2分");
  const count=a.requests.length;await a.h.update();assert.equal(a.requests.length,count);
  await a.emit("click",{closest:s=>s==="[data-sip-history-prev]"});await new Promise(setImmediate);assert.match(a.node("#sip-history-rows").innerHTML,/10001/);
});
test("failures differ from an empty history and retry never exposes raw errors",async()=>{
  const a=setup();a.h.render("account");a.reply(async()=>{throw Error("secret-server-content");});await a.h.update();
  assert.match(a.node("#sip-history-rows").innerHTML,/加载失败/);assert.doesNotMatch(a.node("#sip-history-rows").innerHTML,/secret|暂无通话记录/);assert.equal(a.node("#sip-history-outgoing").textContent,"—");
  a.reply(async()=>({items:[],nextCursor:"",totalMinutes:0,outgoingMinutes:0,incomingMinutes:0}));await a.h.update();assert.match(a.node("#sip-history-rows").innerHTML,/暂无通话记录/);
});
test("date bounds respect local midnight and escape all server-rendered text",async()=>{
  const a=setup();assert.equal(a.h.bounds("2026-02-30"),null);const b=a.h.bounds("2026-09-27");assert.equal(new Date(b.from).getHours(),0);assert.equal(new Date(b.to).getDate(),28);
  a.h.render("account");a.reply(async()=>({items:[{...base,number:"<img src=x>"}],nextCursor:"",totalMinutes:2,outgoingMinutes:2,incomingMinutes:0}));await a.h.update();assert.doesNotMatch(a.node("#sip-history-rows").innerHTML,/<img/);assert.match(a.node("#sip-history-rows").innerHTML,/&lt;img/);
});

test("incoming labels distinguish receiver, caller and historical unknown reasons",()=>{
  const {h}=setup();
  for(const [status,want] of Object.entries({connected:"已接听",active:"通话中",ringing:"来电响铃",no_answer:"未接来电",call_timeout:"接听超时",rejected:"已拒接",busy:"本机忙碌",remote_cancelled:"对方取消",answered_elsewhere:"其他用户已接听",failed:"接听失败",madeup:"接听失败",cancelled:"已取消",unconnected:"未接通",module_error:"模块异常",service_unavailable:"电话服务异常"})) {
    assert.equal(h.status({status,direction:"incoming"}),want);
  }
  assert.equal(h.minutes({...base,direction:"incoming",status:"answered_elsewhere",answeredAt:null}),null);
});
test("missing, negative or invalid direction totals show failure instead of a made-up split",async()=>{
  for(const bad of [undefined,null,-1,1.5,"2",Number.MAX_SAFE_INTEGER+1]) {
    for(const field of ["outgoingMinutes","incomingMinutes"]) {
      const a=setup();a.h.render("account");
      a.reply(async()=>({items:[],nextCursor:"",totalMinutes:0,outgoingMinutes:0,incomingMinutes:0,[field]:bad}));
      await a.h.update();assert.match(a.node("#sip-history-rows").innerHTML,/加载失败/);
      assert.equal(a.node("#sip-history-outgoing").textContent,"—");
      assert.equal(a.node("#sip-history-incoming").textContent,"—");
    }
  }
  const a=setup();a.h.render("account");await a.h.update();
  for(const id of ["outgoing","incoming"]) assert.equal(a.node(`#sip-history-${id}`).textContent,"0小时0分");
});
