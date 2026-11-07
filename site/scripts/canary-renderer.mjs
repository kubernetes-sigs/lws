/** Apply these display-only changes while exporting the recorded canary suite.
 * The standalone renderer checkout stays untouched. Its original and adapted
 * source hashes are retained in every drawing's provenance.
 */
export function canaryRenderer(source) {
  const replace = (before, after) => {
    if (source.split(before).length !== 2) throw new Error('Canary renderer adapter needs review: source changed.');
    source = source.replace(before, after);
  };
  replace("const revisionCaption = indexed.revisions.length > 1 ? ` · ${indexed.revisions.join(' → ')}` : '';",
    "const activeRevisions = indexed.revisions.filter(name => !name.startsWith('cleanup '));\n  const revisionCaption = activeRevisions.length > 1 ? ` · ${activeRevisions.join(' → ')}` : '';");
  const row = 'const row = observed.get(JSON.stringify([revisionName, role]));';
  replace(row, row + "\n    if (revisionName.startsWith('cleanup ') && !row?.revision.pods.length && !row?.revision.requested && !row?.revision.ready) continue;");
  replace('Recorded API observations · time compressed', 'All API observations retained · clock shows presentation time');
  return source;
}
