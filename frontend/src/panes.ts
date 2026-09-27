// Draggable dividers between the three columns.
//
// The layout is a grid whose outer two columns are CSS variables and whose
// middle column is the remainder, so dragging a divider only ever moves that
// one edge: widening the preview takes space from the editor and leaves the
// sidebar alone. Sizes are stored in pixels once dragged, because a percentage
// would re-interpret itself every time the window resized.

export interface PaneSizes {
  sidebar: number;
  preview: number;
}

interface Divider {
  /** The gutter element the reader grabs. */
  gutter: HTMLElement;
  /** Which variable it drives. */
  key: keyof PaneSizes;
  /** Which way the pane grows relative to pointer movement. */
  direction: 1 | -1;
  minimum: number;
}

const STORAGE_KEY = 'lotus.panes';

// Floors, not preferences: below these a pane holds nothing useful, and a
// divider dragged to the edge becomes impossible to grab again.
const MIN_SIDEBAR = 140;
const MIN_PREVIEW = 180;
const MIN_EDITOR = 240;

export function mountPanes(panes: HTMLElement) {
  const stored = read();
  if (stored) apply(panes, stored);

  const dividers: Divider[] = [
    {
      gutter: panes.querySelector<HTMLElement>('#gutter-sidebar')!,
      key: 'sidebar',
      direction: 1,
      minimum: MIN_SIDEBAR,
    },
    {
      // The preview is the right-hand column, so it grows as the pointer moves
      // left.
      gutter: panes.querySelector<HTMLElement>('#gutter-preview')!,
      key: 'preview',
      direction: -1,
      minimum: MIN_PREVIEW,
    },
  ];

  for (const divider of dividers) {
    divider.gutter.addEventListener('pointerdown', (event) => start(panes, dividers, divider, event));
    // Double-click restores the default, which is the only way back once a
    // divider has been dragged somewhere unhelpful.
    divider.gutter.addEventListener('dblclick', () => {
      panes.style.removeProperty(variable(divider.key));
      save(measure(panes));
    });
  }
}

function start(panes: HTMLElement, all: Divider[], divider: Divider, event: PointerEvent) {
  event.preventDefault();
  divider.gutter.setPointerCapture(event.pointerId);
  divider.gutter.classList.add('dragging');
  // While dragging, the pointer is often over the editor or the PDF; without
  // this the cursor flickers and text gets selected under it.
  document.body.classList.add('resizing');

  const startX = event.clientX;
  const startWidth = measure(panes)[divider.key];
  const total = panes.clientWidth;

  const move = (moveEvent: PointerEvent) => {
    const delta = (moveEvent.clientX - startX) * divider.direction;
    const other = all.find((d) => d !== divider)!;
    const otherWidth = measure(panes)[other.key];

    // The editor is the column that absorbs the change, so it sets the ceiling.
    const maximum = Math.max(divider.minimum, total - otherWidth - MIN_EDITOR - gutters(panes));
    const next = Math.min(maximum, Math.max(divider.minimum, startWidth + delta));
    panes.style.setProperty(variable(divider.key), `${Math.round(next)}px`);
  };

  const finish = () => {
    divider.gutter.removeEventListener('pointermove', move);
    divider.gutter.removeEventListener('pointerup', finish);
    divider.gutter.removeEventListener('pointercancel', finish);
    divider.gutter.classList.remove('dragging');
    document.body.classList.remove('resizing');
    save(measure(panes));
  };

  divider.gutter.addEventListener('pointermove', move);
  divider.gutter.addEventListener('pointerup', finish);
  divider.gutter.addEventListener('pointercancel', finish);
}

function variable(key: keyof PaneSizes): string {
  return key === 'sidebar' ? '--w-sidebar' : '--w-preview';
}

/** Reads the columns as they are actually laid out, defaults included. */
function measure(panes: HTMLElement): PaneSizes {
  const columns = getComputedStyle(panes).gridTemplateColumns.split(' ').map(Number.parseFloat);
  return {
    sidebar: columns[0] || MIN_SIDEBAR,
    preview: columns[4] || MIN_PREVIEW,
  };
}

function gutters(panes: HTMLElement): number {
  const columns = getComputedStyle(panes).gridTemplateColumns.split(' ').map(Number.parseFloat);
  return (columns[1] || 0) + (columns[3] || 0);
}

function apply(panes: HTMLElement, sizes: PaneSizes) {
  panes.style.setProperty('--w-sidebar', `${sizes.sidebar}px`);
  panes.style.setProperty('--w-preview', `${sizes.preview}px`);
}

function read(): PaneSizes | null {
  try {
    const raw = localStorage.getItem(STORAGE_KEY);
    if (!raw) return null;
    const parsed = JSON.parse(raw) as Partial<PaneSizes>;
    if (typeof parsed.sidebar !== 'number' || typeof parsed.preview !== 'number') return null;
    // A stored layout from a much wider window would leave no room for the
    // editor; the defaults are better than a squeezed column.
    return { sidebar: Math.max(MIN_SIDEBAR, parsed.sidebar), preview: Math.max(MIN_PREVIEW, parsed.preview) };
  } catch {
    // Private windows, blocked site data, or a corrupt value. The default
    // layout is always a reasonable answer.
    return null;
  }
}

function save(sizes: PaneSizes) {
  try {
    localStorage.setItem(STORAGE_KEY, JSON.stringify(sizes));
  } catch {
    // See read(): losing the layout is not worth failing a drag over.
  }
}
