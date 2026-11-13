// Regenerate only on the Linux validation host with Node 24+.
// Usage: node generate-reference.mjs /path/to/lm-detector /path/to/evidence /path/to/old/octopus
// All expected values come from the pinned upstream TypeScript, never the Go port.
import { createHash } from 'node:crypto';
import { readFile, writeFile, mkdir, copyFile } from 'node:fs/promises';
import { existsSync } from 'node:fs';
import { registerHooks } from 'node:module';
import { dirname, resolve, extname } from 'node:path';
import { fileURLToPath, pathToFileURL } from 'node:url';

const sourceCommit = '2c61f8f836de93d3ce60a9f76f8b8ff9e15011ac';
const [referenceArg, evidenceArg, baselineArg] = process.argv.slice(2);
if (!referenceArg || !evidenceArg || !baselineArg) throw new Error('reference, evidence and baseline directories are required');
const reference = resolve(referenceArg);
const evidence = resolve(evidenceArg);
const baseline = resolve(baselineArg);
const here = dirname(fileURLToPath(import.meta.url));
const data = resolve(here, '../data');
const sha256 = bytes => createHash('sha256').update(bytes).digest('hex');
const readJSON = async path => JSON.parse(await readFile(path, 'utf8'));
const writeJSON = (path, value) => writeFile(path, JSON.stringify(value, null, 2) + '\n');

registerHooks({
  resolve(specifier, context, nextResolve) {
    if (specifier.startsWith('.') && !extname(specifier) && context.parentURL?.startsWith(pathToFileURL(reference + '/').href)) {
      const candidate = new URL(specifier + '.ts', context.parentURL);
      if (existsSync(candidate)) return nextResolve(candidate.href, context);
    }
    return nextResolve(specifier, context);
  }
});
const upstream = path => import(pathToFileURL(resolve(reference, 'shared', path)).href);
const core = await upstream('fingerprint-core.js');
const { decodeDetector } = await upstream('detector-codec.ts');
const { positionalDecision } = await upstream('positional.ts');
const { analyzeSharedOutputs, calibrateRanking, supportsSharedDetector } = await upstream('shared-detector.ts');
const { parseReference } = await upstream('reference.ts');
const detectorBytes = await readFile(resolve(reference, 'data/shared_detector.bin'));
const bankBytes = await readFile(resolve(reference, 'data/unified_bank.json'));
if (sha256(detectorBytes) !== 'fe32914d74896c097c46ab4e8346224a738e59716f4190b7dc667ac639020c5f' ||
    sha256(bankBytes) !== 'dff79cd5d1d381b5d470f4b8dd7ddb58e1ae96b1ac09b6c8b43106aa0a880ad5') {
  throw new Error('Reference data is not the reviewed snapshot; review and update the source pin first');
}
const suiteBytes = await readFile(resolve(reference, 'data/enrollment-suite.json'));
if (sha256(suiteBytes) !== '18ccae737392254cbe3bc274a01d5adc8f896a64e79d2bacac0d3ac1444f40c9') throw new Error('Challenge suite is not the reviewed Git blob');
const bank = JSON.parse(bankBytes.toString('utf8'));
const detector = decodeDetector(detectorBytes);
const referenceBytes = await readFile(resolve(reference, 'data/unified_reference.jsonl'));
if (sha256(referenceBytes) !== bank.reference_sha256 || !supportsSharedDetector(bank, detector)) throw new Error('Mismatched reference, bank or detector');
const batches = parseReference(referenceBytes.toString('utf8'));
const oldData = resolve(baseline, 'internal/modelverify/fingerprint');
const oldBank = await readJSON(resolve(oldData, 'data/models.json'));
const oldDetector = decodeDetector(await readFile(resolve(oldData, 'data/detector.bin')));
const existing = await readJSON(resolve(oldData, 'testdata/g_full.json'));
const inputs = existing.map(({source,texts,expected_count}) => ({source,texts,expected_count}));
const additional = ['gpt-5.5', 'gemini-3.1-pro-preview', 'gemini-4-argon', 'gemini-4-carbon', 'gemini-4-hydrogen'];
for (const id of additional) {
  const samples = batches.filter(batch => batch.model.id === id).flatMap(batch => batch.samples);
  const picked = ['query-01','query-02','query-03'].map(challenge => samples.find(sample => sample.challenge_id === challenge));
  if (picked.some(sample => !sample)) throw new Error('Missing fixed reference samples for ' + id);
  inputs.push({source:id,texts:picked.map(sample => sample.text),expected_count:picked.map(sample => sample.expected_count)});
}
const analyze = (input, bank, artifact) => analyzeSharedOutputs(
  input.texts.map((text,index) => ({text,expected_count:input.expected_count[index]})), bank, artifact, {allowPartial:true});
const summary = analysis => ({
  decision: analysis.decision ?? 'scorable', prediction:analysis.prediction, family:analysis.family_prediction,
  probability:analysis.probability, score:analysis.ranking_score,
  top5:analysis.results.slice(0,5).map(({model,score}) => ({model,score}))
});
const golden = inputs.map(input => ({...input,...summary(analyze(input,bank,detector))}));
if (golden.some(item => item.decision !== 'scorable')) throw new Error('A full golden fixture is not scorable');
const parseInputs = (await readJSON(resolve(oldData,'testdata/g_parse.json'))).map(item => item.in);
const parseGolden = parseInputs.map(input => {
  const numbers = core.parseNumbers(input);
  return {in:input,want:null,got:numbers,counts:core.countNumbers(numbers)};
});
const seq = (await readJSON(resolve(oldData,'testdata/g_feat.json'))).seq;
const featureGolden = {
  seq,hellinger:core.hellingerFeature(core.countNumbers(seq)),ordered:core.orderedBlockFeature(seq),
  robust:core.robustScoreNumbers(seq,bank),positional:positionalDecision(seq,detector.ranker.positional)
};
const oldScores = (await readJSON(resolve(oldData,'testdata/g_calib.json'))).scores;
const scores = detector.model_ids.map((_,index) => oldScores[index % oldScores.length]);
const calibration = {scores,probs:calibrateRanking(scores,detector)};
if (!calibration.probs) throw new Error('Calibration is not bound to the reviewed detector');
const models = {
  schema:'octopus-fingerprint-models-v1',built_at:bank.built_at,reference_sha256:bank.reference_sha256,
  minimum_valid_numbers:bank.minimum_valid_numbers,recommended_queries:bank.recommended_queries,
  models:bank.models.map(({id,display_name,family,family_name,response_count})=>({id,display_name,family,family_name,response_count}))
};
await mkdir(evidence,{recursive:true});
await copyFile(resolve(reference,'data/shared_detector.bin'),resolve(data,'detector.bin'));
await copyFile(resolve(reference,'data/enrollment-suite.json'),resolve(data,'suite.json'));
await writeFile(resolve(data,'models.json'),JSON.stringify(models,null,1) + '\n');
for (const [name,value] of Object.entries({'g_parse.json':parseGolden,'g_feat.json':featureGolden,'g_calib.json':calibration,'g_full.json':golden})) {
  const previous = await readFile(resolve(oldData,'testdata',name),'utf8');
  await writeFile(resolve(here,name),JSON.stringify(value) + (previous.endsWith('\n') ? '\n' : ''));
}
const manifest = {
  source_repository:'https://github.com/Ikaleio/lm-detector',source_commit:sourceCommit,
  bank_built_at:bank.built_at,model_count:bank.models.length,
  detector_sha256:sha256(detectorBytes),bank_sha256:sha256(bankBytes),reference_sha256:bank.reference_sha256,
  suite_sha256:sha256(await readFile(resolve(reference,'data/enrollment-suite.json'))),
  calibration:{schema:detector.calibration?.schema,method:detector.calibration?.method,tau:detector.calibration?.tau,
    base_sha256:detector.calibration?.binding?.base_sha256,reference_sha256:detector.calibration?.binding?.reference_sha256},
  fixture_sources:inputs.map(input => input.source),
  note:'Closed-set relative scores and probabilities are not proof of model identity. All expected values are exported from the reference TypeScript.'
};
await writeJSON(resolve(here,'reference_manifest.json'),manifest);
await writeJSON(resolve(evidence,'catalog-reference-inputs.json'),inputs);
const comparison = inputs.filter(input=>!input.source.startsWith('gemini-4-')).map(input=>({
  source:input.source,before:summary(analyze(input,oldBank,oldDetector)),after:summary(analyze(input,bank,detector))
}));
await writeJSON(resolve(evidence,'catalog-legacy-ranking-comparison.json'),comparison);
console.log(JSON.stringify({source_commit:sourceCommit,model_count:models.models.length,fixtures:golden.map(({source,prediction,family,decision})=>({source,prediction,family,decision})),legacy:comparison.map(({source,before,after})=>({source,before:before.prediction,after:after.prediction,unchanged:before.prediction===after.prediction&&before.family===after.family&&before.decision===after.decision}))},null,2));
