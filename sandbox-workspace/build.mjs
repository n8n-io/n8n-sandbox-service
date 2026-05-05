const filePath = process.argv[2] || './src/workflow.ts';
try {
  const mod = await import(filePath);
  const wf = mod.default;
  if (!wf || typeof wf.toJSON !== 'function') {
    console.log(JSON.stringify({ success: false, errors: ['Default export is not a workflow. Make sure your file has: export default workflow(...)'] }));
    process.exit(1);
  }
  const validation = wf.validate();
  const json = wf.toJSON();
  const warnings = [...(validation.errors || []), ...(validation.warnings || [])];
  // Preserve undefined values as null — newCredential() produces NewCredentialImpl
  // which serializes to undefined in toJSON(). Without this, JSON.stringify drops
  // the credential keys entirely and the server can't resolve them.
  const replacer = (k, v) => v === undefined ? null : v;
  console.log(JSON.stringify({ success: true, workflow: json, warnings }, replacer));
} catch (e) {
  console.log(JSON.stringify({ success: false, errors: [e instanceof Error ? e.message : String(e)] }));
  process.exit(1);
}
