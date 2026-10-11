import { useEffect, useRef, useState, type SetStateAction } from 'react';
export type DraftStatus = 'empty' | 'saved' | 'error';
export function readDraft<T>(key: string, fallback: T, storage?: Pick<Storage, 'getItem'>): { value: T; status: DraftStatus } {
  try {const raw = (storage || localStorage).getItem('tandem.draft.' + key);if (!raw) return {value:fallback,status:'empty'};const parsed=JSON.parse(raw);if (!('value' in parsed) || typeof parsed.value !== typeof fallback) throw Error();return {value:parsed.value as T,status:'saved'};}
  catch {return {value:fallback,status:'error'};}
}
export function writeDraft<T>(key: string, value: T, storage?: Pick<Storage, 'setItem'>): DraftStatus {
  try {(storage || localStorage).setItem('tandem.draft.' + key, JSON.stringify({value, at:Date.now()}));return 'saved';}catch{return 'error';}
}
export function useDraft<T>(key: string, fallback: T): [T, (value: SetStateAction<T>) => void, DraftStatus] {
  const [state, setState] = useState(() => ({key,...readDraft(key,fallback)}));
  const latest = useRef(state);
  latest.current = state;
  useEffect(()=>{const reload=()=>{const next={key,...readDraft(key,fallback)};latest.current=next;setState(next);};window.addEventListener('tandem-drafts',reload);return()=>window.removeEventListener('tandem-drafts',reload);},[key]);
  function change(input: SetStateAction<T>) {
    const old=latest.current.key===key?latest.current.value:readDraft(key,fallback).value;
    const value=typeof input==='function'?(input as (v:T)=>T)(old):input;
    const status=writeDraft(key,value); // persist before returning from the input event
    if(latest.current.key===key){const next={key,value,status};latest.current=next;setState(next);}
  }
  if (state.key !== key) {const next={key,...readDraft(key,fallback)};latest.current=next;setState(next);return [next.value, change, next.status];}
  return [state.value, change, state.status];
}
export function migrateDrafts(from: string, to: string) {
  try {const prefix='tandem.draft.'+from;for(const key of Object.keys(localStorage)){if(key.startsWith(prefix)){const value=localStorage.getItem(key);if(value!==null)localStorage.setItem('tandem.draft.'+to+key.slice(prefix.length),value);}}}catch{/* original remains recoverable */}
}
