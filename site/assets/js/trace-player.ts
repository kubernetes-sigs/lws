/** Unlisted scenario picker. All rendering and playback belong to the shared player. */
import type {Drawing} from './animation-player';
type Entry = {id: string; title: string; source: string};
type Group = {id: string; title: string; scenarios: Entry[]};
const groups: Group[] = JSON.parse(document.querySelector('#trace-player-catalog')!.textContent!);
const figure = document.querySelector<HTMLElement>('[data-animation-source]')!;
const scenarios = document.querySelector<HTMLElement>('[data-trace-scenarios]')!;
const theme = document.querySelector<HTMLSelectElement>('[data-trace-theme]')!;
const allThemes = new URLSearchParams(location.search).get('themes') === 'all';
const fileInput = document.querySelector<HTMLInputElement>('[data-trace-file]')!;
const save = document.querySelector<HTMLButtonElement>('[data-trace-export]')!;
const status = document.querySelector<HTMLElement>('[data-trace-status]')!;
const tabs = [...document.querySelectorAll<HTMLButtonElement>('[data-trace-suite]')];
const mediaButtons = [...document.querySelectorAll<HTMLButtonElement>('[data-trace-media]')];
const exportStatus = document.querySelector<HTMLElement>('[data-trace-export-status]')!;
const cancel = document.querySelector<HTMLButtonElement>('[data-trace-cancel]')!;
let suite = '', requestId = 0, pendingName = '', loading = false;
let selected: Entry | undefined;
let current: {drawing: Drawing; name: string} | undefined;
let imported: typeof current;
let exporting: AbortController | undefined;
theme.closest<HTMLElement>('[data-trace-theme-control]')!.hidden = !allThemes;

function updateHash() {
  const hash = new URLSearchParams(location.hash.slice(1));
  hash.set('suite', suite);
  if (selected) hash.set('scenario', selected.id); else hash.delete('scenario');
  if (allThemes && theme.value) hash.set('theme', theme.value); else hash.delete('theme');
  history.replaceState(null, '', `#${hash}`);
}

function applyTheme(value: string) {
  theme.value = allThemes && [...theme.options].some(option => option.value === value) ? value : '';
  if (theme.value) figure.dataset.animationTheme = theme.value; else delete figure.dataset.animationTheme;
}

function syncExports() {
  save.disabled = loading || !current;
  mediaButtons.forEach(button => {button.disabled = loading || !current || !!exporting;});
  cancel.hidden = !exporting;
}

function load(detail: object, name = selected?.id ?? imported?.name ?? 'imported-animation') {
  pendingName = name;
  loading = true;
  status.textContent = 'Loading…';
  status.classList.remove('visually-hidden');
  syncExports();
  figure.dispatchEvent(new CustomEvent('animation-load', {detail: {...detail, requestId: ++requestId}}));
}

function selectSuite(id: string, scenario?: string) {
  if (id === 'docs' && scenario === 'scaling') scenario = 'scaling-down';
  const group = groups.find(item => item.id === id);
  if (suite !== (group?.id ?? 'import')) {
    scenarios.replaceChildren(...(group?.scenarios ?? []).map(entry => {
      const button = document.createElement('button'), detail = document.createElement('small');
      const [name, ...parts] = entry.title.split(' · ');
      button.type = 'button'; button.dataset.traceScenario = entry.id;
      button.append(document.createTextNode(name));
      if (parts.length) {detail.textContent = parts.join(' · '); button.append(detail);}
      button.addEventListener('click', () => selectSuite(group!.id, entry.id));
      return button;
    }));
    scenarios.closest('nav')!.scrollTop = 0;
  }
  suite = group?.id ?? 'import';
  tabs.forEach(tab => tab.setAttribute('aria-pressed', String(tab.dataset.traceSuite === suite)));
  document.querySelector('[data-trace-list-title]')!.textContent = group ? `${group.title} scenarios` : 'Import JSON';
  document.querySelector<HTMLElement>('[data-trace-empty]')!.hidden = !!group;
  fileInput.closest<HTMLElement>('[data-trace-import]')!.hidden = !!group;
  selected = group?.scenarios.find(entry => entry.id === scenario) ?? group?.scenarios[0];
  if (selected) {
    figure.hidden = false;
    load({source: selected.source});
  } else if (imported) {
    figure.hidden = false;
    load({drawing: imported.drawing});
  } else {
    ++requestId; // Ignore a pending selection if the user switches to the empty import workspace.
    current = undefined;
    loading = false;
    figure.hidden = true;
    syncExports();
    status.textContent = 'Choose a prepared drawing JSON file to play it locally.';
    status.classList.remove('visually-hidden');
  }
  scenarios.querySelectorAll<HTMLElement>('[data-trace-scenario]').forEach(button =>
    button.setAttribute('aria-current', String(button.dataset.traceScenario === selected?.id)));
  updateHash();
}

tabs.forEach(tab => tab.addEventListener('click', () => selectSuite(tab.dataset.traceSuite!)));
theme.addEventListener('change', () => {applyTheme(theme.value); updateHash();});
fileInput.addEventListener('change', () => {
  const file = fileInput.files?.[0];
  if (file) {
    load({file}, file.name.replace(/\.json(?:\.gz)?$/i, ''));
  }
  fileInput.value = '';
});

figure.addEventListener('animation-ready', event => {
  const detail = (event as CustomEvent).detail;
  if (detail.requestId !== requestId) return;
  current = {drawing: detail.drawing, name: pendingName};
  loading = false;
  const available = current.drawing.themes ?? ['lws', 'lws-dark'];
  [...theme.options].forEach(option => {option.disabled = !!option.value && !available.includes(option.value);});
  if (theme.value && !available.includes(theme.value)) {applyTheme(''); updateHash();}
  if (suite === 'import') imported = current;
  figure.hidden = false;
  syncExports();
  status.textContent = selected?.title ?? `Imported: ${current.name}`;
  status.classList.add('visually-hidden');
});
figure.addEventListener('animation-error', event => {
  const detail = (event as CustomEvent).detail;
  if (detail.requestId !== requestId) return;
  loading = false;
  syncExports();
  status.textContent = `${detail.message} The last successfully loaded drawing is unchanged.`;
  status.classList.remove('visually-hidden');
});
function download(blob: Blob, name: string, extension: string) {
  const link = document.createElement('a');
  link.href = URL.createObjectURL(blob);
  link.download = `${name.replace(/[^a-z0-9_-]+/gi, '-').slice(0, 120) || 'animation'}.${extension}`;
  link.click();
  setTimeout(() => URL.revokeObjectURL(link.href), 1000);
}
save.addEventListener('click', () => {
  if (current) download(new Blob([JSON.stringify(current.drawing)], {type: 'application/json'}), current.name, 'json');
});
cancel.addEventListener('click', () => exporting?.abort());
mediaButtons.forEach(button => button.addEventListener('click', async () => {
  if (!current || exporting) return;
  const source = current, controller = exporting = new AbortController();
  const format = button.dataset.traceMedia === 'gif' ? 'gif' : 'video';
  const speed = Number(figure.querySelector<HTMLSelectElement>('[data-animation-speed]')!.value);
  const palette = figure.dataset.animationTheme, dark = document.documentElement.dataset.bsTheme === 'dark';
  syncExports();
  exportStatus.textContent = 'Preparing export…';
  try {
    const assets = JSON.parse(document.querySelector('#trace-export-assets')!.textContent!);
    const {exportScenario, videoSupport} = await import(assets.module);
    const label = format === 'gif' ? 'GIF' : (await videoSupport())?.extension.toUpperCase() ?? 'video';
    const result = await exportScenario({drawing: source.drawing, format, speed,
      theme: palette, dark, signal: controller.signal, gifWorkerURL: assets.worker, videoModuleURL: assets.video,
      onProgress: (fraction: number) => {
        exportStatus.textContent = `${label}: ${Math.round(fraction * 100)}%`;
      }});
    download(result.blob, source.name, result.extension);
    exportStatus.textContent = `${label} exported at 1080p using the selected theme and ${speed}× speed.`;
  } catch (error) {
    exportStatus.textContent = controller.signal.aborted ? 'Export cancelled.' : `Export failed: ${(error as Error).message}`;
  } finally {exporting = undefined; syncExports();}
}));
function restore() {
  const hash = new URLSearchParams(location.hash.slice(1));
  applyTheme(hash.get('theme') ?? '');
  const requested = hash.get('suite');
  selectSuite(requested === 'import' || groups.some(group => group.id === requested) ? requested! : groups[0].id,
    hash.get('scenario') ?? undefined);
}
window.addEventListener('hashchange', restore);
restore();
