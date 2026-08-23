import { execFileSync } from 'node:child_process'
import { mkdirSync } from 'node:fs'
import { dirname, join, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'

const webDir = resolve(dirname(fileURLToPath(import.meta.url)), '..')
const repoDir = resolve(webDir, '..')
const triple = process.env.TAURI_ENV_TARGET_TRIPLE || hostTriple()
const target = goTarget(triple)
const extension = target.goos === 'windows' ? '.exe' : ''
const output = join(webDir, 'src-tauri', 'binaries', `opsnerva-${triple}${extension}`)

mkdirSync(dirname(output), { recursive: true })
execFileSync('go', [
  'build', '-buildvcs=false', '-trimpath', '-ldflags=-s -w', '-o', output, './cmd/opsnerva',
], {
  cwd: repoDir,
  env: { ...process.env, CGO_ENABLED: '0', GOOS: target.goos, GOARCH: target.goarch },
  stdio: 'inherit',
})
console.log(`Prepared OpsNerva sidecar: ${output}`)

function hostTriple() {
  const details = execFileSync('rustc', ['-vV'], { encoding: 'utf8' })
  const match = details.match(/^host:\s*(\S+)$/m)
  if (!match) throw new Error('Unable to determine the Rust host target triple')
  return match[1]
}

function goTarget(value) {
  const architectures = new Map([
    ['x86_64', 'amd64'],
    ['aarch64', 'arm64'],
  ])
  const architecture = architectures.get(value.split('-')[0])
  const goos = value.includes('-windows-')
    ? 'windows'
    : value.includes('-linux-')
      ? 'linux'
      : value.includes('-apple-')
        ? 'darwin'
        : ''
  if (!architecture || !goos) throw new Error(`Unsupported desktop target: ${value}`)
  return { goos, goarch: architecture }
}
