import { readFileSync, writeFileSync } from 'node:fs'
import { probeTokenizer } from './ref.mjs'
const bank = JSON.parse(readFileSync('../refs/lm-detector/data/tokenizer_bank.json', 'utf8'))
let seed = 12345
const rnd = () => { seed |= 0; seed = seed + 0x6D2B79F5 | 0; let t = Math.imul(seed ^ seed >>> 15, 1 | seed); t = t + Math.imul(t ^ t >>> 7, 61 | t) ^ t; return ((t ^ t >>> 14) >>> 0) / 4294967296 }
const claims = ['', 'gpt-5.5', 'claude-opus-5', 'deepseek-v4-pro', 'openai/gpt-4o', 'qwen3.5-plus', 'glm-5', 'gemini-3-pro', 'kimi-k2.5', 'llama-3.1-70b']
const cases = []
const add = (name, model, base, table) => cases.push({ name, model, baseline: base, table })
bank.classes.forEach((c, i) => {
  const H = 11 + (i % 7)
  const table = {}; bank.probes.forEach((p, j) => { table[p.id] = H + c.counts[j] })
  add(`exact-${c.id}`, claims[i % claims.length], [H, H], table)
})
for (let k = 0; k < 12; k++) {
  const c = bank.classes[Math.floor(rnd() * bank.classes.length)]
  const H = 20 + k
  const table = {}; bank.probes.forEach((p, j) => { const d = rnd() < 0.1 ? (rnd() < 0.5 ? -1 : 1) * (1 + Math.floor(rnd() * 2)) : 0; table[p.id] = H + c.counts[j] + d })
  add(`noisy-${k}-${c.id}`, claims[k % claims.length], [H, H], table)
}
for (let k = 0; k < 6; k++) {
  const c = bank.classes[Math.floor(rnd() * bank.classes.length)]
  const H = 30 + k
  const table = {}; bank.probes.forEach((p, j) => { const d = rnd() < 0.4 ? 3 + Math.floor(rnd() * 4) : 0; table[p.id] = H + c.counts[j] + d })
  add(`relative-${k}-${c.id}`, claims[(k + 3) % claims.length], [H, H], table)
}
for (let k = 0; k < 6; k++) {
  const H = 9 + k
  const table = {}; bank.probes.forEach((p, j) => { table[p.id] = H + 3 + Math.floor(rnd() * 60) })
  add(`alien-${k}`, claims[(k + 1) % claims.length], [H, H], table)
}
for (const [cid, model] of [['o200k','gpt-5.5'],['deepseek-v3','deepseek-v4-pro'],['qwen3.5','qwen3.5-plus'],['gemma3','gemini-3-pro'],['llama3','meta-llama/llama-3.1-70b-instruct'],['cl100k','gpt-4o']]) {
  const c = bank.classes.find(x => x.id === cid); const H = 14; const table = {}; bank.probes.forEach((p, j) => { table[p.id] = H + c.counts[j] }); add(`claim-${cid}-${model.replace('/', '_')}`, model, [H, H], table)
}
{ const c = bank.classes[0]; const H = 15; const table = {}; bank.probes.forEach((p, j) => { table[p.id] = H + c.counts[j] }); add('drift', 'gpt-5', [H, H + 2], table) }
const out = []
for (const cs of cases) {
  let baseCalls = 0
  const textToId = new Map(bank.probes.map(p => [bank.wrapper.prefix + p.text + bank.wrapper.suffix, p.id]))
  const transport = async (url, config, body) => {
    const prompt = body.messages[0].content
    let tokens
    if (prompt === bank.wrapper.prefix + bank.wrapper.suffix) { tokens = cs.baseline[Math.min(baseCalls, cs.baseline.length - 1)]; baseCalls++ }
    else tokens = cs.table[textToId.get(prompt)]
    return new Response(JSON.stringify({ model: 'x', usage: { prompt_tokens: tokens } }), { headers: { 'content-type': 'application/json' } })
  }
  const run = await probeTokenizer({ format: 'openai', baseUrl: 'https://x', model: cs.model, apiKey: 'k', stream: false }, bank, { url: 'https://x', transport, concurrency: 1 })
  if (run.error) throw run.error
  out.push({ ...cs, expect: {
    steps: run.steps.map(s => s.probe),
    drift: run.baselineDrift,
    verdict: run.verdict,
    unknown: run.posterior.unknown, alien: run.posterior.alien,
    classes: run.posterior.classes.slice(0, 5),
    labs: run.posterior.labs.slice(0, 3),
  } })
}
writeFileSync('golden.json', JSON.stringify({ source: 'lm-detector@175355e shared/tokenizer-probe.ts probeTokenizer(concurrency=1)', cases: out }))
console.log('cases', out.length, 'mean requests', out.reduce((s, c) => s + c.expect.steps.length, 0) / out.length)
const kinds = {}; out.forEach(c => { const k = c.name.split('-')[0] + ':' + c.expect.verdict.kind + ':' + (c.expect.verdict.claim?.status ?? '-'); kinds[k] = (kinds[k] || 0) + 1 }); console.log(kinds)
let correct = 0; out.filter(c => c.name.startsWith('exact')).forEach(c => { if (c.expect.verdict.top.id === c.name.slice(6)) correct++ }); console.log('exact top correct', correct)
