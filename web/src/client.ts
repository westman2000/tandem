let token = '';
export function setToken(value: string) { token = value; }
export class UncertainAction extends Error { receiptKey:string;constructor(message:string,receiptKey:string){super(message);this.name='UncertainAction';this.receiptKey=receiptKey;} }
export function allowNewAttempt(key:string){memory.delete(key);try{localStorage.removeItem(key);}catch{}}
const memory = new Map<string, string>();
export async function fingerprint(path: string, method: string, body?: unknown): Promise<string> {
  const parts: unknown[] = [method, path];
  if (body instanceof FormData) {
    for (const [name, value] of body.entries()) {
      if (typeof value === 'string') parts.push([name, value]);
      else parts.push([name, value.name, value.type, await digest(await value.arrayBuffer())]);
    }
  } else parts.push(body ?? null);
  return digest(new TextEncoder().encode(JSON.stringify(parts)));
}
async function digest(bytes: BufferSource) { return Array.from(new Uint8Array(await crypto.subtle.digest('SHA-256', bytes))).map(v => v.toString(16).padStart(2, '0')).join(''); }
function actionID(key: string) {
  let id = memory.get(key);
  try { id ||= localStorage.getItem(key) || undefined; } catch { /* memory still protects this tab */ }
  if (!id) id = crypto.randomUUID();
  memory.set(key, id); try { localStorage.setItem(key, id); } catch { /* retained in memory */ }
  return id;
}
export async function api<T>(path: string, method = 'GET', body?: unknown): Promise<T> {
  const form = body instanceof FormData;
  const key = method !== 'GET' ? 'tandem.request.' + await fingerprint(path, method, body) : '';
  const id = key ? actionID(key) : '';
  for (let attempt = 0; attempt < 2; attempt++) {
    const controller = new AbortController(), timeout = setTimeout(() => controller.abort(), method === 'GET' ? 12000 : 60000);
    let response: Response; let value: any;
    try { response = await fetch(`/api/${path}`, { method, signal: controller.signal, headers: { ...(id ? { 'X-Tandem-Token': token, 'X-Tandem-Request': id } : {}), ...(!form && body ? { 'Content-Type': 'application/json' } : {}) }, body: body ? (form ? body : JSON.stringify(body)) : undefined }); value = await response.json(); }
    catch { throw new Error(method === 'GET' ? 'Нет связи с Tandem. Проверяем соединение…' : 'Подтверждение от Tandem не получено. Обновите состояние проекта и повторите то же действие: сохранённый ID защитит от дубликата.'); }
    finally { clearTimeout(timeout); }
    if (response.status === 403 && id && attempt === 0) {
      const fresh = await api<{ token: string }>('bootstrap');
      if (fresh.token !== token) { token = fresh.token; continue; }
    }
    // Lost/truncated response or an uncertain server outcome keeps its receipt.
    if (value !== null && !response.headers.has('X-Tandem-Pending')) {memory.delete(key);try{localStorage.removeItem(key);}catch{}}
    if (!response.ok) {if(response.headers.has('X-Tandem-Uncertain')){const issue=new UncertainAction(value?.error||'Результат действия неизвестен',key);if(typeof window!=='undefined')window.dispatchEvent(new CustomEvent('tandem-uncertain',{detail:issue}));throw issue;}throw new Error(value?.error || `Ошибка ${response.status}`);}
    if (value === null) throw new Error('Ответ Tandem оборвался. Обновите состояние проекта; повтор того же действия защищён от дубликата.');
    return value as T;
  }
  throw new Error('Не удалось восстановить подключение к Tandem. Обновите страницу.');
}
