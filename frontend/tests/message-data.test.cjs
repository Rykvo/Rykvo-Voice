const {test}=require('node:test'),assert=require('node:assert/strict'),vm=require('node:vm'),{readFileSync}=require('node:fs'),{join}=require('node:path');
function setup(){const timers=new Map(),requests=[],events={};let clock=Date.now(),next=0,answer={items:[],cursor:0,more:false},windowAnswer={items:[],earlier:false,newer:false},windowCalls=[],deleteCalls=[];const context=vm.createContext({Date:{now:()=>clock},AbortController,Map,Set,document:{hidden:false,addEventListener:(k,f)=>events[k]=f,querySelector:()=>null},window:{addEventListener(){}},setTimeout:f=>{timers.set(++next,f);return next},clearTimeout:id=>timers.delete(id),Http:{apiURL:p=>'/api'+p},Backend:{enabled:()=>true,messages:{removeThreads:async o=>{deleteCalls.push(o);return {snapshot:1234,more:deleteCalls.length<2}},window:async o=>{windowCalls.push(o);return typeof windowAnswer==="function"?windowAnswer(o):windowAnswer},search:async()=>({items:[]}),threads:async o=>{requests.push(o);if(answer instanceof Error)throw answer;return answer},send:async o=>({...o.body,id:'message-1',revision:2,image:'message-1'}),saveContact:async o=>({...o.body,revision:20}),remove:async()=>{}}}});vm.runInContext(readFileSync(join(__dirname,'../message-data.js'),'utf8'),context);return {data:vm.runInContext('MessageData',context),requests,timers,events,answer:v=>answer=v,windowAnswer:v=>windowAnswer=v,windowCalls,deleteCalls,advance:ms=>clock+=ms}}
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

test('multi-page synchronization keeps its snapshot and resets expired cursors', async()=>{
 const f=setup(),changes=[];
 f.answer({items:[{id:'old',revision:1}],cursor:1,snapshot:9,more:true});
 const stop=f.data.subscribe(items=>changes.push(items)); await new Promise(setImmediate);
 const tick=async()=>{const run=[...f.timers.values()][0];f.timers.clear();await run()};
 f.answer({items:[{id:'b',revision:4}],cursor:9,snapshot:9,more:false});await tick();
 assert.equal(f.requests.at(-1).query.snapshot,9);
 const error=new Error('expired');error.code='CURSOR_EXPIRED';f.answer(error);await tick();
 assert.equal(changes.at(-1).length,0);
 f.answer({items:[{id:'new',revision:11}],cursor:11,snapshot:11,more:false});await tick();
 assert.equal(f.requests.at(-1).query.after,0);assert.equal(f.requests.at(-1).query.snapshot,undefined);
 assert.deepEqual(Array.from(changes.at(-1),r=>r.id),['new']);stop();
});

const message = (i,number="+12025550123") => ({id:"message-"+i,revision:i+1,senderId:"module-01",lineId:"card-1",number,text:"message "+i,at:i});
const thread = number => ({remotePaged:true,senderId:"module-01",lineId:"card-1",numbers:[number || "+12025550123"]});

test('one conversation summary replaces old previews without accumulating history',async()=>{
 const f=setup(),snapshots=[];f.answer({items:[message(1000)],cursor:1001});const stop=f.data.subscribe((items,contacts,meta)=>snapshots.push({items,meta}));await new Promise(setImmediate);
 for(let i=1001;i<1100;i++){const tick=[...f.timers.values()][0];f.timers.clear();f.answer({items:[message(i)],cursor:i+1});await tick()}
 assert.equal(snapshots.at(-1).items.length,1);assert.equal(snapshots.at(-1).items[0].id,'message-1099');assert.equal(f.windowCalls.length,0);stop();
});

test('history pages replace the active window and use exact message cursors',async()=>{
 const f=setup();let current;const stop=f.data.subscribe((items,c,meta)=>current=meta.history);
 f.windowAnswer({items:Array.from({length:80},(_,i)=>message(920+i)),earlier:true,newer:false});await f.data.watch(thread());
 assert.equal(current.items.length,80);assert.equal(current.items[0].id,'message-920');
 f.windowAnswer({items:Array.from({length:80},(_,i)=>message(840+i)),earlier:true,newer:true});await f.data.page('earlier');
 assert.equal(f.windowCalls.at(-1).query.anchor,'message-920');assert.equal(current.items.length,80);assert.equal(current.items[0].id,'message-840');assert.equal(current.pinned,true);
 f.windowAnswer({items:Array.from({length:80},(_,i)=>message(920+i)),earlier:true,newer:false});await f.data.page('latest');
 assert.equal(f.windowCalls.at(-1).query.anchor,'');assert.equal(current.items.length,80);stop();
});

test('switching conversations cancels a stale history response',async()=>{
 const f=setup();let current,resolve;const stop=f.data.subscribe((items,c,meta)=>current=meta.history);
 f.windowAnswer(()=>new Promise(r=>resolve=r));const old=f.data.watch(thread());
 f.windowAnswer({items:[message(2,'+12025550456')],earlier:false,newer:false});await f.data.watch(thread('+12025550456'));
 assert.equal(f.windowCalls[0].signal.aborted,true);resolve({items:[message(1)]});await old;
 assert.equal(current.items[0].number,'+12025550456');stop();
});

test('summary tombstone removes the thread and old snapshots do not resurrect it',async()=>{
 const f=setup();let current;f.answer({items:[message(1)],cursor:2});const stop=f.data.subscribe(items=>current=items);await new Promise(setImmediate);
 const tick=async answer=>{f.answer(answer);const fn=[...f.timers.values()][0];f.timers.clear();await fn()};
 await tick({items:[{...message(2),id:'',deleted:true}],cursor:3});assert.equal(current.length,0);
 await tick({items:[message(1)],cursor:3});assert.equal(current.length,0);stop();
});

test('a send response can be corrected by the authoritative equal-revision summary',async()=>{
 const f=setup();let items;const stop=f.data.subscribe(v=>items=v);
 await f.data.send({senderId:'module-01',lineId:'card-1',number:'+12025550123',state:'accepted',text:'older retry',at:1});
 const tick=[...f.timers.values()][0];f.timers.clear();
 f.answer({items:[{...message(1),id:'newer-preview',text:'latest preview',at:2}],cursor:2});await tick();
 assert.equal(items[0].id,'newer-preview');assert.equal(items[0].text,'latest preview');stop();
});

test('deleting a conversation includes unloaded history and reuses one cutoff',async()=>{
 const f=setup();const stop=f.data.subscribe(()=>{});await new Promise(setImmediate);
 await f.data.removeThreads({moduleId:'module-01',lineId:'card-1',numbers:['+12025550123']});
 assert.equal(f.deleteCalls.length,2);assert.equal(f.deleteCalls[0].body.snapshot,0);assert.equal(f.deleteCalls[1].body.snapshot,1234);
 assert.equal(f.requests.at(-1).query.after,0);stop();
});

test('an in-flight page is reconciled again if a summary changes before it returns',async()=>{
 const f=setup();let resolve;const stop=f.data.subscribe(()=>{});await new Promise(setImmediate);
 f.windowAnswer(()=>new Promise(r=>resolve=r));const loading=f.data.watch(thread());
 const tick=[...f.timers.values()][0];f.timers.clear();f.answer({items:[message(999)],cursor:1000});await tick();
 f.windowAnswer({items:[message(999)],earlier:true,newer:false});resolve({items:[message(998)],earlier:true,newer:false});await loading;await new Promise(setImmediate);
 assert.equal(f.windowCalls.length,2);stop();
});

test('recent conversations reopen from bounded memory without another request',async()=>{
 const f=setup();let current;const stop=f.data.subscribe((items,c,meta)=>current=meta.history);await new Promise(setImmediate);
 f.windowAnswer({items:[message(1)]});await f.data.watch(thread());
 f.windowAnswer({items:[message(2,'+12025550456')]});await f.data.watch(thread('+12025550456'));
 const calls=f.windowCalls.length;await f.data.watch(thread());
 assert.equal(f.windowCalls.length,calls);assert.equal(current.items[0].id,'message-1');assert.equal(current.busy,false);
 for(let i=0;i<13;i++){const number='+1202555'+String(1000+i);f.windowAnswer({items:[message(i,number)]});await f.data.watch(thread(number));}
 f.windowAnswer({items:[message(3)]});await f.data.watch(thread());assert.equal(f.windowCalls.length,calls+14);stop();
});

test('summary changes invalidate inactive cache and preserve card/module isolation',async()=>{
 const f=setup();let current;const stop=f.data.subscribe((items,c,meta)=>current=meta.history);await new Promise(setImmediate);
 f.windowAnswer({items:[message(1)]});await f.data.watch(thread());
 f.windowAnswer({items:[{...message(2),lineId:'card-2'}]});await f.data.watch({...thread(),lineId:'card-2'});
 const tick=[...f.timers.values()][0];f.timers.clear();f.answer({items:[message(3)],cursor:4});await tick();
 f.windowAnswer({items:[message(3)]});await f.data.watch(thread());assert.equal(current.items[0].id,'message-3');assert.equal(f.windowCalls.length,3);
 await f.data.removeThreads({moduleId:'module-01',lineId:'card-1',numbers:thread().numbers});
 f.windowAnswer({items:[]});await f.data.watch(thread());assert.equal(current.items.length,0);assert.equal(f.windowCalls.length,4);stop();
});

test('a cached switch aborts old requests and never accepts their late response',async()=>{
 const f=setup();let current,resolve;const stop=f.data.subscribe((items,c,meta)=>current=meta.history);await new Promise(setImmediate);
 f.windowAnswer({items:[message(1)]});await f.data.watch(thread());
 f.windowAnswer(()=>new Promise(r=>resolve=r));const pending=f.data.watch(thread('+12025550456'));
 await f.data.watch(thread());assert.equal(f.windowCalls.at(-1).signal.aborted,true);
 resolve({items:[message(9,'+12025550456')]});await pending;assert.equal(current.items[0].id,'message-1');stop();
});

test('history toolbar does not flash loading text during background updates',()=>{
 const source=readFileSync(join(__dirname,'../messages.js'),'utf8');
 assert.doesNotMatch(source,/读取中/);assert.match(source,/history.hidden = !earlier && !newer && !remote\?\.error;/);
});

test('stale cached history stays visible while revalidation runs and shows real failure',async()=>{
 const f=setup();let current,resolve;const stop=f.data.subscribe((items,c,meta)=>current=meta.history);await new Promise(setImmediate);
 f.windowAnswer({items:[message(1)]});await f.data.watch(thread());
 f.windowAnswer({items:[message(2,'+12025550456')]});await f.data.watch(thread('+12025550456'));f.advance(31000);
 f.windowAnswer(()=>new Promise(r=>resolve=r));const pending=f.data.watch(thread());
 assert.equal(current.items[0].id,'message-1');assert.equal(current.busy,true);
 resolve({items:[message(3)]});await pending;assert.equal(current.items[0].id,'message-3');
 f.windowAnswer({items:[message(2,'+12025550456')]});await f.data.watch(thread('+12025550456'));f.advance(31000);
 f.windowAnswer(()=>Promise.reject(new Error('offline')));await f.data.watch(thread());
 assert.equal(current.items[0].id,'message-3');assert.match(current.error,/读取失败/);stop();
});
