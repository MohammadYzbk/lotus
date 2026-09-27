// The Git sheet: branch, commit, push, open a pull request.
//
// One panel rather than four scattered controls, because these are steps of a
// single workflow and the interesting question is almost always "where am I and
// what is uncommitted". It borrows the same shell as the GitHub sheets so the
// app has one dialog language.

import {
  AbortRebase,
  CommitChanges,
  ContinueRebase,
  CreateBranch,
  FetchRemote,
  OpenPullRequest,
  PullChanges,
  PushBranch,
  RebaseOnRemote,
  RepositoryBranches,
  RepositoryState,
  SwitchBranch,
  SyncStatus,
} from '../wailsjs/go/main/App';
import { BrowserOpenURL } from '../wailsjs/runtime/runtime';
import type { main, vcs } from '../wailsjs/go/models';

/** The one-line summary of where the branch stands against origin. */
function describeSync(sync: vcs.Sync): string {
  if (sync.rebasing) return 'Rebase in progress';
  if (!sync.tracking) return 'Not on origin yet';
  if (sync.diverged) return `Diverged — ${sync.ahead} local, ${sync.behind} remote`;
  if (sync.behind > 0) return `${sync.behind} commit${sync.behind === 1 ? '' : 's'} to pull`;
  if (sync.ahead > 0) return `${sync.ahead} commit${sync.ahead === 1 ? '' : 's'} to push`;
  return 'Up to date with origin';
}

/** Opens a modal and returns a handle for closing it. */
function sheet(label: string): { panel: HTMLDivElement; close: () => void } {
  const overlay = document.createElement('div');
  overlay.className = 'palette-overlay';
  overlay.innerHTML = `<div class="palette sheet" role="dialog" aria-modal="true" aria-label="${label}"></div>`;
  document.body.append(overlay);

  const panel = overlay.querySelector<HTMLDivElement>('.sheet')!;
  const close = () => overlay.remove();

  overlay.addEventListener('mousedown', (event) => {
    if (event.target === overlay) close();
  });
  overlay.addEventListener('keydown', (event) => {
    if (event.key === 'Escape') {
      event.preventDefault();
      close();
    }
  });
  return { panel, close };
}

function element<T extends HTMLElement>(html: string): T {
  const template = document.createElement('template');
  template.innerHTML = html.trim();
  return template.content.firstElementChild as T;
}

function button(label: string, className: string, onClick: () => void): HTMLButtonElement {
  const node = document.createElement('button');
  node.className = className;
  node.textContent = label;
  node.addEventListener('click', onClick);
  return node;
}

function escape(text: string): string {
  const node = document.createElement('span');
  node.textContent = text;
  return node.innerHTML;
}

/**
 * Shows the Git sheet.
 *
 * `onChanged` fires after anything that moves the working copy, so the branch
 * badge in the header stays honest without the sheet knowing about it.
 */
export async function openGitSheet(onChanged: () => void) {
  const state = await RepositoryState();
  if (!state.repository) {
    const { panel, close } = sheet('Git');
    panel.append(
      element(`
        <div class="sheet-body">
          <h2 class="sheet-title">Not a repository</h2>
          <p class="sheet-text">This project is a plain folder. Open a GitHub repository to work with branches and commits.</p>
        </div>
      `),
    );
    const actions = element<HTMLDivElement>('<div class="sheet-actions"></div>');
    actions.append(button('Close', 'btn', close));
    panel.append(actions);
    return;
  }

  const { panel, close } = sheet('Git');

  /** Re-reads everything and redraws, after any operation. */
  const refresh = async (note?: { text: string; bad?: boolean }) => {
    const [current, branches, sync] = await Promise.all([
      RepositoryState(),
      RepositoryBranches(),
      SyncStatus(),
    ]);
    onChanged();
    draw(current, branches, sync.sync, note);
  };

  const run = async (action: Promise<main.GitResult>) => {
    const result = await action;
    await refresh(
      result.error ? { text: result.error, bad: true } : { text: result.detail || 'Done' },
    );
  };

  /** Same, for the calls that report sync state instead. */
  const runSync = async (action: Promise<main.SyncResult>) => {
    const result = await action;
    await refresh(
      result.error ? { text: result.error, bad: true } : { text: result.detail || 'Done' },
    );
  };

  function draw(
    current: vcs.State,
    branches: main.BranchList,
    sync: vcs.Sync,
    note?: { text: string; bad?: boolean },
  ) {
    panel.replaceChildren();

    const where = current.branch || (current.unborn ? 'no commits yet' : 'detached');
    const body = element<HTMLDivElement>(`
      <div class="sheet-body">
        <h2 class="sheet-title">On <span class="git-branch">${escape(where)}</span></h2>
        <p class="sheet-text">${
          current.dirty ? 'There are uncommitted changes.' : 'Everything is committed.'
        }</p>
      </div>
    `);
    panel.append(body);

    body.append(element(`<p class="sheet-sync">${escape(describeSync(sync))}</p>`));

    // A stopped rebase is the only state that blocks everything else, so it
    // gets the loudest treatment and its own pair of actions.
    if (sync.rebasing) {
      const stalled = element<HTMLDivElement>(`
        <div class="sheet-note bad">
          <strong>A rebase stopped on conflicts.</strong>
          ${
            sync.conflicted?.length
              ? `Open ${sync.conflicted.map((f) => `<code>${escape(f)}</code>`).join(', ')},
                 remove the &lt;&lt;&lt;&lt;&lt;&lt;&lt; markers, then continue.`
              : 'Resolve the conflicts, then continue.'
          }
        </div>
      `);
      body.append(stalled);

      const rebaseActions = element<HTMLDivElement>('<div class="sheet-row"></div>');
      rebaseActions.append(
        button('Abort', 'btn btn-quiet', () => void runSync(AbortRebase())),
        button('Continue', 'btn', () => void runSync(ContinueRebase())),
      );
      body.append(rebaseActions);
    }

    if (note) body.append(element(`<p class="sheet-note ${note.bad ? 'bad' : 'info'}">${escape(note.text)}</p>`));

    // --- branch -----------------------------------------------------------
    const branchRow = element<HTMLDivElement>(`
      <div class="sheet-field">
        <label class="sheet-label">Branch</label>
        <div class="sheet-row"></div>
      </div>
    `);
    const row = branchRow.querySelector<HTMLDivElement>('.sheet-row')!;

    const picker = document.createElement('select');
    picker.className = 'sheet-input sheet-select';
    for (const name of branches.local ?? []) {
      const option = document.createElement('option');
      option.value = name;
      option.textContent = name;
      option.selected = name === current.branch;
      picker.append(option);
    }
    picker.addEventListener('change', () => void run(SwitchBranch(picker.value)));
    row.append(picker, button('New…', 'btn', () => promptForBranch()));
    body.append(branchRow);

    const promptForBranch = () => {
      const form = element<HTMLDivElement>(`
        <div class="sheet-field">
          <label class="sheet-label" for="branch-name">New branch from here</label>
          <input class="sheet-input" id="branch-name" spellcheck="false" autocomplete="off"
                 placeholder="feature/something" />
        </div>
      `);
      const input = form.querySelector<HTMLInputElement>('input')!;
      input.addEventListener('keydown', (event) => {
        if (event.key !== 'Enter') return;
        event.preventDefault();
        void run(CreateBranch(input.value));
      });
      branchRow.replaceWith(form);
      input.focus();
    };

    // --- commit -----------------------------------------------------------
    const commitField = element<HTMLDivElement>(`
      <div class="sheet-field">
        <label class="sheet-label" for="commit-message">Commit message</label>
        <textarea class="sheet-input sheet-textarea" id="commit-message" rows="3"
                  placeholder="Describe the change"></textarea>
      </div>
    `);
    const message = commitField.querySelector<HTMLTextAreaElement>('textarea')!;
    // Cmd-Enter commits, the convention everywhere a message box means "and go".
    message.addEventListener('keydown', (event) => {
      if (event.key !== 'Enter' || !(event.metaKey || event.ctrlKey)) return;
      event.preventDefault();
      void run(CommitChanges(message.value));
    });
    body.append(commitField);

    // --- actions ----------------------------------------------------------
    const actions = element<HTMLDivElement>('<div class="sheet-actions"></div>');
    actions.append(button('Close', 'btn btn-quiet', close));

    const commit = button('Commit', 'btn', () => void run(CommitChanges(message.value)));
    commit.disabled = !current.dirty;
    commit.title = current.dirty ? 'Commit everything uncommitted' : 'Nothing to commit';

    const push = button('Push', 'btn', () => void run(PushBranch()));

    const fetch = button('Fetch', 'btn btn-quiet', () => void runSync(FetchRemote()));
    fetch.title = 'Check origin for new commits — changes nothing locally';

    // Pull only appears when there is something to pull; rebase only when a
    // fast-forward is impossible. Offering both always would mean one of them
    // is usually the wrong thing to press.
    const catchUp = sync.diverged
      ? button('Rebase onto origin', 'btn', () => void runSync(RebaseOnRemote()))
      : button('Pull', 'btn', () => void runSync(PullChanges()));
    catchUp.disabled = sync.rebasing || (!sync.diverged && sync.behind === 0);
    catchUp.title = sync.diverged
      ? 'Replay your commits on top of origin'
      : sync.behind > 0
        ? `Fast-forward ${sync.behind} commit${sync.behind === 1 ? '' : 's'}`
        : 'Nothing to pull';

    const pull = button('Pull request…', 'btn', () => {
      void (async () => {
        const result = await OpenPullRequest(message.value.split('\n')[0], message.value);
        if (result.error) {
          await refresh({ text: result.error, bad: true });
          return;
        }
        // The browser is where a pull request is actually read and merged.
        BrowserOpenURL(result.pullRequest.url);
        close();
      })();
    });
    pull.title = 'Open a pull request for this branch on GitHub';

    // Disabled rather than hidden while a rebase is stalled: the buttons stay
    // where they were, and the reason is stated above them.
    for (const control of [commit, push, pull]) control.disabled ||= sync.rebasing;

    actions.append(fetch, catchUp, commit, push, pull);
    panel.append(actions);

    if (current.dirty) message.focus();
  }

  const [branches, initial] = await Promise.all([RepositoryBranches(), SyncStatus()]);
  draw(state, branches, initial.sync);
}
