import { writeFileSync } from 'node:fs';
import { resolve } from 'node:path';
import { defineConfig, type Plugin } from 'vite';

// main.go has `//go:embed all:frontend/dist`, which refuses to compile when that
// directory does not exist — so a fresh clone cannot run `go build` unless
// something in dist is tracked. The build output itself is gitignored, so the
// repo keeps an empty dist/.gitkeep.
//
// The catch: `vite build` empties outDir first, which deletes that placeholder.
// Left alone it shows up as a spurious deletion in `git status` and gets tidied
// away, and the next fresh clone stops building. So put it back after each build.
function keepDistDirTracked(): Plugin {
  let outDir = '';
  return {
    name: 'lotus:keep-dist-dir-tracked',
    apply: 'build',
    configResolved(config) {
      outDir = resolve(config.root, config.build.outDir);
    },
    closeBundle() {
      writeFileSync(resolve(outDir, '.gitkeep'), '');
    },
  };
}

export default defineConfig({
  plugins: [keepDistDirTracked()],
});
