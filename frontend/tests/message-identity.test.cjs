const {test} = require('node:test');
const assert = require('node:assert/strict');
const vm = require('node:vm');
const {readFileSync} = require('node:fs');
const {join} = require('node:path');
const context = vm.createContext({});
for (const file of ['countries.js', 'message-identity.js']) vm.runInContext(readFileSync(join(__dirname,'..',file),'utf8'),context);
const identity = vm.runInContext('MessageIdentity',context);
test('all requested international/national aliases resolve without altering source records',()=>{
  const groups = [
    ['+86123456789','86123456789','123456789'],
    ['+447598999919','07598999919','7598999919'],
    ['+85262717066','852 62717066','62717066'],
    ['+13322500550','13322500550','3322500550'],
  ];
  const items=groups.flat().map(number=>({number,lineId:'line-a'}));
  const before=JSON.stringify(items),resolve=identity.resolver(items);
  let offset=0;
  for(const group of groups){for(const item of items.slice(offset,offset+group.length)) assert.equal(resolve(item),group[0]);offset+=group.length;}
  assert.equal(JSON.stringify(items),before);
});
test('no guessing countries, suffix matching, cross-SIM merging or short-code rewriting',()=>{
  const items=['+441234567890','+491234567890','1234567890','567890','54623','#DIYsim','13322500550'].map(number=>({number,lineId:'a'}));
  items.push({number:'+13322500550',lineId:'b'});
  const resolve=identity.resolver(items);
  for(const item of items) assert.equal(resolve(item),item.number);
  assert.notEqual(identity.threadId({senderId:'m1',lineId:'a',number:'123'}),identity.threadId({senderId:'m1',lineId:'b',number:'123'}));
});
test('00 notation, spaces, hyphens, original leading zeros and text senders survive',()=>{
  const items=['0044 7598-999919','07598999919','+39 0212345678','0212345678','212345678','AB-CD'].map(number=>({number,lineId:'a'}));
  const resolve=identity.resolver(items);
  assert.equal(resolve(items[0]),'+447598999919');assert.equal(resolve(items[1]),'+447598999919');
  assert.equal(resolve(items[3]),'+390212345678');assert.equal(resolve(items[4]),'212345678');assert.equal(resolve(items[5]),'AB-CD');
});
test('avatars are stable, varied and prefer remark characters',()=>{
  assert.equal(identity.avatar('+13322500550').text,'50');
  assert.equal(identity.avatar('+13322500550','张先生').text,'张先');
  assert.equal(identity.avatar('#DIYsim').text,'DI');
  assert.equal(identity.avatar('+13322500550').color,identity.avatar('+13322500550','张先生').color);
  assert.ok(new Set(Array.from({length:20},(_,i)=>identity.avatar('+133225005'+i).color)).size>3);
});
test('only a delivery receipt is shown as delivered; pending states have spinner',()=>{
  assert.equal(identity.delivery({mine:true,state:'waiting_network',kind:'mms'}),'failed');
  assert.equal(identity.delivery({mine:true,state:'sending',kind:'mms'}),'pending');
  for(const state of ['queued','sending','waiting_network']) assert.equal(identity.delivery({mine:true,state}),'pending');
  for(const state of ['unknown','partial','new-state']) assert.equal(identity.delivery({mine:true,state}),'failed');
  assert.equal(identity.delivery({mine:true,state:'accepted'}),'failed');
  assert.equal(identity.delivery({mine:true,state:'accepted',kind:'sms'}),'pending');
  assert.equal(identity.delivery({mine:true,state:'unknown',kind:'sms',issue:'SMS_DELIVERY_UNCONFIRMED'}),'failed');
  for(const state of ['failed','expired','cancelled']) assert.equal(identity.delivery({mine:true,state}),'failed');
  assert.equal(identity.delivery({mine:true,state:'delivered'}),'delivered');
  assert.equal(identity.delivery({mine:false,state:'received'}),'');
});
