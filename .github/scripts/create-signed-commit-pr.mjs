#!/usr/bin/env node
// Create a branch commit via GitHub GraphQL (auto-signed) and open a PR.
//
// Usage:
//   node .github/scripts/create-signed-commit-pr.mjs \
//     --base <base-branch> \
//     --branch <head-branch> \
//     --title <pr-title> \
//     --body <pr-body> | --body-file <path> \
//     --label <label> \
//     --message <commit-headline> \
//     -- <file>...
//
// Requires: gh, zx (npm ci --prefix .github/scripts). Auth via GH_TOKEN / GITHUB_TOKEN.

import { readFileSync, existsSync } from 'node:fs'
import { $ } from 'zx'

$.verbose = true

const USAGE =
  'usage: create-signed-commit-pr.mjs --base BASE --branch BRANCH --title TITLE (--body BODY|--body-file PATH) --label LABEL --message MESSAGE -- FILE...'

function parseArgs(argv) {
  // Drop leading non-flag args (script path when invoked via `zx script.mjs ...`).
  let i = 0
  while (i < argv.length && !argv[i].startsWith('-')) i++

  const out = {
    base: '',
    branch: '',
    title: '',
    body: '',
    bodyFile: '',
    label: '',
    message: '',
    files: [],
  }

  for (; i < argv.length; i++) {
    const arg = argv[i]
    const next = () => {
      const v = argv[++i]
      if (v === undefined) {
        console.error(`missing value for ${arg}`)
        console.error(USAGE)
        process.exit(2)
      }
      return v
    }

    switch (arg) {
      case '--base':
        out.base = next()
        break
      case '--branch':
        out.branch = next()
        break
      case '--title':
        out.title = next()
        break
      case '--body':
        out.body = next()
        break
      case '--body-file':
        out.bodyFile = next()
        break
      case '--label':
        out.label = next()
        break
      case '--message':
        out.message = next()
        break
      case '--':
        out.files.push(...argv.slice(i + 1))
        return out
      default:
        console.error(`unknown argument: ${arg}`)
        console.error(USAGE)
        process.exit(2)
    }
  }
  return out
}

const args = parseArgs(process.argv.slice(2))
if (args.bodyFile) {
  args.body = readFileSync(args.bodyFile, 'utf8')
}

if (!args.base || !args.branch || !args.title || !args.body || !args.message || args.files.length < 1) {
  console.error(USAGE)
  process.exit(2)
}

const repo = process.env.GITHUB_REPOSITORY
if (!repo) {
  console.error('GITHUB_REPOSITORY is required')
  process.exit(1)
}

// Validate and encode file payloads before any remote ref mutation so a
// missing/unreadable file cannot wipe an existing PR branch.
const additions = []
for (const filePath of args.files) {
  if (!existsSync(filePath)) {
    console.error(`file not found: ${filePath}`)
    process.exit(1)
  }
  additions.push({
    path: filePath,
    contents: readFileSync(filePath).toString('base64'),
  })
}

async function refSha(ref) {
  const path = `repos/${repo}/git/ref/heads/${ref}`
  const result = await $`gh api ${path} --jq .object.sha`.nothrow()
  if (result.exitCode !== 0) return ''
  return result.stdout.trim()
}

async function setBranchSha(sha) {
  const path = `repos/${repo}/git/refs/heads/${args.branch}`
  await $`gh api ${path} --method PATCH -f sha=${sha} -F force=true --jq .object.sha`
}

async function createBranch(sha) {
  const path = `repos/${repo}/git/refs`
  await $`gh api ${path} --method POST -f ref=${`refs/heads/${args.branch}`} -f sha=${sha} --jq .object.sha`
}

async function deleteBranch() {
  const path = `repos/${repo}/git/refs/heads/${args.branch}`
  await $`gh api ${path} --method DELETE`.nothrow()
}

async function hasOpenPr(branch) {
  const result =
    await $`gh pr list --repo ${repo} --head ${branch} --state open --json number --limit 1`.nothrow()
  if (result.exitCode !== 0) return false
  const prs = JSON.parse(result.stdout || '[]')
  return prs.length > 0
}

const baseSha = await refSha(args.base)
if (!baseSha) {
  console.error(`base branch not found: ${args.base}`)
  process.exit(1)
}

let previousSha = await refSha(args.branch)
const openPr = await hasOpenPr(args.branch)

// Closed/abandoned head branches must not block retries (replaces
// peter-evans delete-branch: true). Delete when there is no open PR.
if (previousSha && !openPr) {
  console.error(`deleting orphaned branch ${args.branch} (no open PR)`)
  await deleteBranch()
  previousSha = ''
}

const createdBranch = !previousSha

// Point the PR branch at the current base tip so retries stay a single
// signed commit on latest base. createCommitOnBranch requires the ref to
// exist with expectedHeadOid === baseSha.
if (previousSha) {
  await setBranchSha(baseSha)
} else {
  await createBranch(baseSha)
}

const payload = {
  query: `mutation($input: CreateCommitOnBranchInput!) {
    createCommitOnBranch(input: $input) { commit { oid } }
  }`,
  variables: {
    input: {
      branch: {
        repositoryNameWithOwner: repo,
        branchName: args.branch,
      },
      message: { headline: args.message },
      fileChanges: { additions },
      expectedHeadOid: baseSha,
    },
  },
}

try {
  const raw = await $({
    input: JSON.stringify(payload),
  })`gh api graphql --input -`
  const response = JSON.parse(raw.stdout)
  const oid = response?.data?.createCommitOnBranch?.commit?.oid ?? ''
  if (!oid) {
    console.error('createCommitOnBranch failed:')
    console.error(JSON.stringify(response, null, 2))
    throw new Error('createCommitOnBranch returned no oid')
  }
} catch (err) {
  // Restore prior tip (or delete a newly created empty branch) so a failed
  // retry does not leave the PR branch stuck at base with no commit.
  try {
    if (previousSha) {
      console.error(`restoring ${args.branch} to ${previousSha}`)
      await setBranchSha(previousSha)
    } else if (createdBranch) {
      console.error(`deleting empty branch ${args.branch}`)
      await deleteBranch()
    }
  } catch (restoreErr) {
    console.error(`failed to recover ${args.branch} after commit error:`, restoreErr)
  }
  if (err instanceof Error && err.message === 'createCommitOnBranch returned no oid') {
    process.exit(1)
  }
  throw err
}

if (!openPr) {
  const createArgs = [
    'pr',
    'create',
    '--repo',
    repo,
    '--base',
    args.base,
    '--head',
    args.branch,
    '--title',
    args.title,
    '--body',
    args.body,
  ]
  if (args.label) {
    createArgs.push('--label', args.label)
  }
  await $`gh ${createArgs}`
}
