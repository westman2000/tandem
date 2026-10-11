import { test } from 'node:test';
import assert from 'node:assert/strict';
import { renderToStaticMarkup } from 'react-dom/server';
import { AgentHealthCard, RecoveryPanel, type Agent } from '../src/health';
const base:Agent={agent:'claude',installed:true,authenticated:true,subscription:true,version:'fixture',detail:'',loginCommand:'',health:{quotaMode:'events',checkedAt:new Date().toISOString()}};
test('unknown quota is labelled unknown and never rendered as a full allowance',()=>{
 const html=renderToStaticMarkup(<AgentHealthCard agent={base} onCheck={()=>{}}/>);assert.match(html,/доступность не проверена/);assert.match(html,/Проверить доступность/);assert.match(html,/Расходует лимит подписки/);assert.doesNotMatch(html,/Осталось 100%|health-dot available/);
});
test('successful probe without telemetry confirms the response, not quota percentage',()=>{
 const at=new Date().toISOString();const agent={...base,health:{...base.health!,lastSuccessAt:at,accessCheck:{state:'available',model:'sonnet',actualModel:'claude-sonnet-test',startedAt:at,checkedAt:at,detail:'Ответ получен.'}}};
 const html=renderToStaticMarkup(<AgentHealthCard agent={agent} onCheck={()=>{}}/>);assert.match(html,/Последний запрос выполнен · лимит неизвестен/);assert.match(html,/Ответ получен/);assert.match(html,/claude-sonnet-test/);assert.match(html,/disabled=""/);assert.doesNotMatch(html,/Осталось 100%/);
});
test('check is visibly running and cannot be sent twice',()=>{
 const html=renderToStaticMarkup(<AgentHealthCard agent={base} onCheck={()=>{}} checking/>);assert.match(html,/Проверяем доступность Claude/);assert.match(html,/disabled=""/);
});
test('stale windows show age and a passed reset does not claim recovery',()=>{
 const at=new Date(Date.now()-600000).toISOString();const agent={...base,health:{...base.health!,quota:{source:'fixture',checkedAt:new Date().toISOString(),allowed:false,windows:[{id:'weekly',label:'7 дней',checkedAt:at,state:'blocked',usedPercent:100,resetsAt:Math.floor(Date.now()/1000)-60}]}}};
 const html=renderToStaticMarkup(<AgentHealthCard agent={agent}/>);assert.match(html,/Осталось 0%/);assert.match(html,/ожидаем подтверждение/);assert.match(html,/Последнее значение/);assert.doesNotMatch(html,/health-dot available/);
});
test('recovery explains persisted work and offers explicit continuation',()=>{
 const run={id:'run',status:'paused',tasks:[{id:'one',agent:'codex',stage:'draft',status:'done'},{id:'two',agent:'claude',stage:'review',status:'pending'}],recovery:{reason:'restart',at:new Date().toISOString(),detail:'Последняя версия сохранена.'}};
 const html=renderToStaticMarkup(<RecoveryPanel run={run} busy={false} act={async()=>true}/>);assert.match(html,/после перезапуска/);assert.match(html,/Завершено этапов: 1 из 2/);assert.match(html,/Продолжить сохранённую работу/);
});
