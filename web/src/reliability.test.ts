import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readDraft, writeDraft } from './drafts.ts';
import { api, fingerprint, setToken, UncertainAction, allowNewAttempt } from './client.ts';

function storage() {
 const values=new Map<string,string>();
 return {getItem:(key:string)=>values.get(key)??null,setItem:(key:string,value:string)=>{values.set(key,value);},removeItem:(key:string)=>{values.delete(key);}};
}
test('drafts survive reload, stay separate by project/run, and report write failures',()=>{
 const saved=storage();assert.equal(writeDraft('project-a.run-1','Моя идея',saved),'saved');writeDraft('project-b.run-1','Другая идея',saved);
 assert.equal(readDraft('project-a.run-1','',saved).value,'Моя идея');assert.equal(readDraft('project-b.run-1','',saved).value,'Другая идея');assert.equal(readDraft('project-a.run-2','',saved).status,'empty');
 saved.setItem('tandem.draft.bad','{broken');assert.equal(readDraft('bad','',saved).status,'error');assert.equal(writeDraft('key','unsaved',{setItem(){throw Error('quota')}}),'error');
});
test('file fingerprints are stable across picker objects and change with file content',async()=>{
 const make=(value:string)=>{const f=new FormData();f.append('role','reference');f.append('file',new File([value],'idea.md',{type:'text/markdown'}));return f;};
 const a=await fingerprint('upload','POST',make('one')),b=await fingerprint('upload','POST',make('one')),c=await fingerprint('upload','POST',make('two'));assert.equal(a,b);assert.notEqual(a,c);
});
test('lost mutation response reuses receipt; confirmed subsequent action gets a new ID',async()=>{
 const original=globalThis.fetch;Object.defineProperty(globalThis,'localStorage',{value:storage(),configurable:true});setToken('token');const ids:string[]=[];let calls=0;
 globalThis.fetch=(async (_url,init)=>{ids.push((init!.headers as Record<string,string>)['X-Tandem-Request']);if(calls++===0)throw Error('offline');return new Response(JSON.stringify({id:'saved'}),{status:201});}) as typeof fetch;
 try{await assert.rejects(api('projects/test/messages','POST',{text:'hello'}));await api('projects/test/messages','POST',{text:'hello'});await api('projects/test/messages','POST',{text:'hello'});assert.equal(ids[0],ids[1]);assert.notEqual(ids[1],ids[2]);}finally{globalThis.fetch=original;}
});
test('uncertain receipt stays pinned and server restart refreshes CSRF without changing action ID',async()=>{
 const original=globalThis.fetch;Object.defineProperty(globalThis,'localStorage',{value:storage(),configurable:true});setToken('old');const ids:string[]=[];let mutation=0;
 globalThis.fetch=(async (url,init)=>{if(String(url).endsWith('bootstrap'))return new Response(JSON.stringify({token:'new'}));ids.push((init!.headers as Record<string,string>)['X-Tandem-Request']);mutation++;if(mutation===1)return new Response(JSON.stringify({error:'restart'}),{status:403});if(mutation===2)return new Response(JSON.stringify({error:'uncertain'}),{status:409,headers:{'X-Tandem-Pending':'true'}});return new Response(JSON.stringify({ok:true}));}) as typeof fetch;
 try{await assert.rejects(api('projects','POST',{title:'Recovery'}),/uncertain/);await api('projects','POST',{title:'Recovery'});assert.equal(new Set(ids).size,1);}finally{globalThis.fetch=original;}
});

test('uncertain action can only receive a new receipt after explicit reconciliation',async()=>{
 const original=globalThis.fetch;Object.defineProperty(globalThis,'localStorage',{value:storage(),configurable:true});const ids:string[]=[];
 globalThis.fetch=(async(_url,init)=>{ids.push((init!.headers as Record<string,string>)['X-Tandem-Request']);return new Response(JSON.stringify({error:'uncertain'}),{status:409,headers:{'X-Tandem-Pending':'true','X-Tandem-Uncertain':'true'}});}) as typeof fetch;
 try{let pending:UncertainAction|undefined;try{await api('reconciliation','POST',{value:1});}catch(e){assert.ok(e instanceof UncertainAction);pending=e;};assert.ok(pending);allowNewAttempt(pending.receiptKey);await assert.rejects(api('reconciliation','POST',{value:1}));assert.notEqual(ids[0],ids[1]);}finally{globalThis.fetch=original;}
});
