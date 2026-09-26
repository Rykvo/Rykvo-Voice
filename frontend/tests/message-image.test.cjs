const {test}=require('node:test');
const assert=require('node:assert/strict');
const vm=require('node:vm');
const {readFileSync}=require('node:fs');
const {join}=require('node:path');
const source=readFileSync(join(__dirname,'../messages.js'),'utf8');
function setup(){
 const events={},nodes=new Map(),readers=[],toasts=[];
 const node=s=>{if(!nodes.has(s))nodes.set(s,{value:'',innerHTML:'',style:{},classList:{toggle(){}},setAttribute(){}});return nodes.get(s)};
 const context=vm.createContext({
  atob,UI:{$:node,escape:x=>x,toast:x=>toasts.push(x),read:(key,fallback)=>fallback},
  Lines:{valid:()=>false},document:{addEventListener:(type,fn)=>(events[type]??=[]).push(fn)},
  FileReader:class {constructor(){readers.push(this)}readAsDataURL(file){this.file=file}},
 });
 vm.runInContext(source,context);
 function pick(name,size,data,type=''){
  const picker=node('#msg-file');picker.id='msg-file';picker.value='chosen';picker.files=[{name,size,type}];
  for(const fn of events.change||[])fn({target:picker});
  const reader=readers.at(-1);
  if(reader&&reader.file===picker.files[0]){reader.result=`data:${type||'application/octet-stream'};base64,${Buffer.from(data,'latin1').toString('base64')}`;return reader}
 }
 return {pick,node,toasts,context,readers,emit:(type,event)=>(events[type]||[]).forEach(fn=>fn(event))};
}
test('file chooser lists only explicit single-image extensions',()=>{
 const input=source.match(/<input hidden id="msg-file"[^>]+>/)[0];
 assert.match(input,/accept="\.jpg,\.jpeg,\.png,\.gif"/);
 assert.doesNotMatch(input,/multiple|webp|image\//);
});
test('JPG JPEG PNG GIF survive unchanged including absent or alias MIME',()=>{
 for(const [name,data,mime,kind] of [['photo.JPG','\xff\xd8\xffdata','','jpeg'],['photo.jpeg','\xff\xd8\xffdata','image/jpg','jpeg'],['photo.png','\x89PNG\r\n\x1a\ndata','image/png','png'],['photo.GIF','GIF89aframes','image/gif','gif']]){
  const f=setup(),r=f.pick(name,1024*1024,data,mime);r.onload();
  assert.equal(f.toasts.length,0);assert.equal(f.node('#msg-file').value,'');
  assert.match(f.node('#msg-attachment').innerHTML,new RegExp('data:image/'+kind+';base64,'));
  assert.ok(f.node('#msg-attachment').innerHTML.includes(Buffer.from(data,'latin1').toString('base64')));
 }
});
test('oversize empty and unsupported attachments stop before file reading',()=>{
 for(const [name,size] of [['a.jpg',1048577],['a.png',0],['a.webp',10],['a.jfif',10],['a.mp4',10],['a.png.exe',10]]){
  const f=setup();assert.equal(f.pick(name,size,'data'),undefined);assert.equal(f.readers.length,0);assert.equal(f.toasts.length,1);
 }
});
test('renaming unsupported bytes never creates a preview',()=>{
 for(const [name,data] of [['fake.jpg','RIFFxxxxWEBP'],['fake.png','\xff\xd8\xffdata'],['fake.gif','GIF90a']]){
  const f=setup();f.pick(name,20,data).onload();assert.equal(f.toasts.at(-1),'图片格式与内容不符');assert.equal(f.node('#msg-attachment').innerHTML,'');
 }
});
test('latest selection wins and detached views never receive stale previews',()=>{
 const f=setup(),a=f.pick('a.jpg',20,'\xff\xd8\xfffirst'),b=f.pick('b.png',20,'\x89PNG\r\n\x1a\nsecond');
 b.onload();const html=f.node('#msg-attachment').innerHTML;a.onload();assert.equal(f.node('#msg-attachment').innerHTML,html);
 const c=f.pick('c.gif',20,'GIF89aframes');vm.runInContext('Messages.unmount()',f.context);c.onload();assert.equal(f.node('#msg-attachment').innerHTML,html);
});
