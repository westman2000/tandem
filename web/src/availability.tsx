import { useId } from 'react';

export type Availability = { policy: string; unavailableAgent?: string };
export type Review = { ready: boolean; summary: string; issues: string[]; manualChecks: string[] };
export type AvailabilityRun = {
 id: string; status: string; mode?: string; availabilityPolicy?: string; needsReview?: boolean;
 unavailable?: Record<string, { kind: string; detail: string; since: string }>;
 audits?: { id: string; agent: string; artifactTaskId: string; buildId?: string; finishedAt?: string; review?: Review }[];
 tasks: { id: string; agent: string; stage: string; status: string }[];
};
const names: Record<string, string> = { codex: 'Codex', claude: 'Claude Code' };

export function reviewText(text: string): string {
 try {
  const review = JSON.parse(text.replace(/^```(?:json)?\s*/, '').replace(/\s*```$/, ''));
  if (typeof review.summary !== 'string') return text;
  return review.summary + (review.issues?.length ? '\n\nЗамечания:\n' + review.issues.map((s: string) => '- ' + s).join('\n') : '') + (review.manualChecks?.length ? '\n\nПроверить вручную:\n' + review.manualChecks.map((s: string) => '- ' + s).join('\n') : '');
 } catch { return text; }
}

export function AvailabilityFields({ value, change, disabled }: { value: Availability; change: (v: Availability) => void; disabled?: boolean }) {
 const id = useId();
 return <div className="availability-fields"><h3>Если участник недоступен</h3><div className="form-grid">
  <label htmlFor={id + '-policy'}>Поведение<select id={id + '-policy'} value={value.policy} disabled={disabled} onChange={e => change({ ...value, policy: e.target.value })}><option value="solo">Продолжить доступным агентом</option><option value="wait">Остановиться и ждать</option></select></label>
  <label htmlFor={id + '-agent'}>Уже знаете о лимите?<select id={id + '-agent'} value={value.unavailableAgent || ''} disabled={disabled} onChange={e => change({ ...value, unavailableAgent: e.target.value })}><option value="">Оба могут участвовать</option><option value="claude">Claude сейчас недоступен</option><option value="codex">Codex сейчас недоступен</option></select></label>
 </div><p className="muted">Один агент сможет продолжить работу и самопроверку. Независимое ревью можно запросить позже. Лимиты итераций сохраняются.</p></div>;
}

export function AvailabilityPanel({ run, busy, act, buildId }: { run: AvailabilityRun; busy: boolean; act: (path: string, body?: unknown) => Promise<boolean>; buildId?: string }) {
 if (run.mode === 'demo') return null;
 const active = ['running', 'pausing'].includes(run.status);
 const policy = run.availabilityPolicy || 'wait';
 const artifact = [...run.tasks].reverse().find(t => t.status === 'done' && ['draft', 'revision', 'prototype_plan_final', 'prototype_code'].includes(t.stage));
 const canReview = ['completed', 'stopped', 'awaiting_plan'].includes(run.status) && !!artifact;
 const settings = (body: unknown) => act(`runs/${run.id}/availability`, body);
 const audits = (run.audits || []).filter(a => a.review && (!buildId || a.buildId === buildId));
 return <section className="availability-panel" aria-label="Доступность участников">
  {run.needsReview && <p className="review-debt"><strong>Ожидает независимого ревью.</strong> Самопроверка автора сохраняется отдельно.</p>}
  <details open={Object.keys(run.unavailable || {}).length > 0}>
   <summary>Участники · {policy === 'solo' ? 'продолжать при недоступности' : 'ждать обоих'}</summary>
   <label>Если участник недоступен<select value={policy} disabled={busy || active} onChange={e => void settings({ policy: e.target.value })}><option value="solo">Продолжить доступным агентом</option><option value="wait">Остановиться и ждать</option></select></label>
   {['codex', 'claude'].map(agent => <div className="availability-agent" key={agent}><div><strong>{names[agent]}</strong><p>{run.unavailable?.[agent]?.detail || 'Участие разрешено. Остаток лимита неизвестен.'}</p></div><button className="button secondary" disabled={busy || active} onClick={() => void settings({ policy, ...(run.unavailable?.[agent] ? { returnAgent: agent } : { unavailableAgent: agent }) })}>{run.unavailable?.[agent] ? 'Доступ восстановлен' : 'Временно отключить'}</button></div>)}
   {active && <p className="muted">Для изменения состава команды сначала поставьте работу на паузу.</p>}
   {['paused', 'failed'].includes(run.status) && <p className="muted">После изменения настроек нажмите «Продолжить». Сохранённые этапы повторяться не будут.</p>}
  </details>
  {canReview && <div className="audit-request"><button className="button secondary" disabled={busy} onClick={() => void act(`runs/${run.id}/independent-review`, { buildId: buildId || '' })}>Запросить независимое ревью{buildId ? ' этой сборки' : ''}</button><p className="muted">Один запрос второму агенту после восстановления доступа. Он проверит сохранённый результат; файлы останутся прежними.</p></div>}
  {audits.length > 0 && <details className="audit-history" open><summary>Независимые проверки ({audits.length})</summary>{audits.map(a => <article key={a.id}><strong>{names[a.agent]} · {a.review!.ready && !a.review!.issues?.length ? 'Критерии выполнены' : 'Есть замечания'}</strong><small>Версия {a.artifactTaskId.slice(0, 8)} · {new Date(a.finishedAt!).toLocaleString('ru')}</small><p>{a.review!.summary}</p>{a.review!.issues?.length > 0 && <ul>{a.review!.issues.map((v, i) => <li key={i}>{v}</li>)}</ul>}{a.review!.manualChecks?.length > 0 && <><strong>Проверить вручную</strong><ul>{a.review!.manualChecks.map((v, i) => <li key={i}>{v}</li>)}</ul></>}</article>)}</details>}
 </section>;
}
