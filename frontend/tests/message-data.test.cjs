const {test}=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),{readFileSync}=require('node:fs'),{join}=require('node:path');
function setup(){const timers=new Map(),requests=[],events={};let next=0,answer={items:[],cursor:0,more:false};const context=vm.createContext({AbortController,Map,Set,document:{hidden:false,addEventListener:(k,f)=>events[k]=f,querySelector:()=>null},window:{addEventListener(){}},setTimeout:f=>{timers.set(++next,f);return next},clearTimeout:id=>timers.delete(id),Http:{apiURL:p=>'/api'+p},Backend:{enabled:()=>true,messages:{list:async o=>{requests.push(o);return answer},send:async o=>({...o.body,id:'message-1',revision:2,image:'message-1'}),saveContact:async o=>({...o.body,revision:20}),remove:async()=>{}}}});vm.runInContext(readFileSync(join(__dirname,'../message-data.js'),'utf8'),context);return {data:vm.runInContext('MessageData',context),requests,timers,events,answer:v=>answer=v}}
test('message synchronization deduplicates revisions and has one cancellable timer',async()=>{const f=setup(),snapshots=[];f.answer({items:[{id:'a',revision:1,image:'a'}],cursor:1,more:false});const stop=f.data.subscribe(v=>snapshots.push(v));await new Promise(setImmediate);assert.equal(f.timers.size,1);assert.equal(snapshots.at(-1)[0].image,'/api/messages/a/image');const run=[...f.timers.values()][0];f.timers.clear();f.answer({items:[{id:'a',revision:1,image:'a'}],cursor:1});await run();assert.equal(snapshots.length,2);stop();assert.equal(f.timers.size,0)});
test('send result is tracked without turning acceptance into delivery',async()=>{const f=setup();const states=[];const stop=f.data.subscribe(v=>states.push(v));await f.data.send({state:'accepted',text:'test'});assert.equal(states.at(-1)[0].state,'accepted');assert.notEqual(states.at(-1)[0].state,'delivered');stop()});


test('older polling snapshots never undo a saved or cleared note',async()=>{
 const f=setup(),names=[];
 f.answer({items:[],cursor:0,contacts:[{lineId:'a',number:'+13322500550',name:'旧备注',revision:1}]});
 const stop=f.data.subscribe((items,contacts)=>names.push(contacts.map(c=>({...c}))));
 await new Promise(setImmediate);
 await f.data.saveContact({lineId:'a',number:'+13322500550',name:''});
 const run=[...f.timers.values()][0];f.timers.clear();await run();
 assert.equal(names.at(-1)[0].name,'');assert.equal(names.at(-1)[0].revision,20);
 stop();
});
