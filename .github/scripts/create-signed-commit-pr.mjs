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

import { $, fs, minimist, retry } from 'zx'

$.verbose = true
$.timeout = '60s'

const USAGE =
  'usage: create-signed-commit-pr.mjs --base BASE --branch BRANCH --title TITLE (--body BODY|--body-file PATH) --label LABEL --message MESSAGE -- FILE...'

const args = minimist(process.argv.slice(2), {
  string: ['base', 'branch', 'title', 'body', 'body-file', 'label', 'message'],
})
const files = args._
const body = args['body-file'] ? fs.readFileSync(args['body-file'], 'utf8') : args.body

if (!args.base || !args.branch || !args.title || !body || !args.message || files.length < 1) {
  console.error(USAGE)
  process.exit(2)
}

const PROTECTED_BRANCHES = new Set(['main'])
if (PROTECTED_BRANCHES.has(args.branch) || args.branch === args.base) {
  console.error(`refusing to mutate protected branch: ${args.branch}`)
  process.exit(2)
}

const repo = process.env.GITHUB_REPOSITORY
if (!repo) {
  console.error('GITHUB_REPOSITORY is required')
  process.exit(1)
}

// Validate and encode file payloads before any remote ref mutation so a
// missing/unreadable file cannot wipe an existing PR branch.
const additions = files.map((filePath) => {
  if (!fs.existsSync(filePath)) {
    console.error(`file not found: ${filePath}`)
    process.exit(1)
  }
  return {
    path: filePath,
    contents: fs.readFileSync(filePath).toString('base64'),
  }
})

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

/** Force-move the PR branch only if it still points at expectedSha (best-effort CAS). */
async function setBranchShaFrom(expectedSha, sha) {
  const current = await refSha(args.branch)
  if (current !== expectedSha) {
    throw new Error(
      `ref heads/${args.branch} moved concurrently (expected ${expectedSha}, got ${current || '(missing)'})`,
    )
  }
  await setBranchSha(sha)
}

async function createBranch(sha) {
  const path = `repos/${repo}/git/refs`
  await $`gh api ${path} --method POST -f ref=${`refs/heads/${args.branch}`} -f sha=${sha} --jq .object.sha`
}

async function deleteBranch() {
  const path = `repos/${repo}/git/refs/heads/${args.branch}`
  const result = await $`gh api ${path} --method DELETE`.nothrow()
  if (result.exitCode === 0) return
  // Already deleted is success; anything else must surface for retry/recovery.
  if (!(await refSha(args.branch))) return
  throw new Error(
    `failed to delete heads/${args.branch}: ${result.stderr || result.stdout || `exit ${result.exitCode}`}`,
  )
}

async function hasOpenPr(branch) {
  const result =
    await $`gh pr list --repo ${repo} --head ${branch} --state open --json number --limit 1`
  return JSON.parse(result.stdout || '[]').length > 0
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
  await setBranchShaFrom(previousSha, baseSha)
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
  await retry(3, '1s', async () => {
    const tip = await refSha(args.branch)
    // Timeout/5xx after the mutation applied (or a concurrent run won): tip
    // has moved off base — do not re-issue the mutation.
    if (tip && tip !== baseSha) return tip
    if (tip !== baseSha) {
      throw new Error(`expected tip ${baseSha} before commit, got ${tip || '(missing)'}`)
    }

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
    return oid
  })
} catch (err) {
  // Only roll back if the tip is still the base SHA we moved to. Re-reading
  // avoids restoring a stale previousSha over a commit another run just wrote.
  try {
    await retry(5, '1s', async () => {
      const tip = await refSha(args.branch)
      if (previousSha && tip === baseSha) {
        console.error(`restoring ${args.branch} to ${previousSha}`)
        await setBranchShaFrom(baseSha, previousSha)
      } else if (createdBranch && tip === baseSha) {
        console.error(`deleting empty branch ${args.branch}`)
        await deleteBranch()
      } else {
        console.error(
          `not rolling back ${args.branch}: tip is ${tip || '(missing)'} (base ${baseSha})`,
        )
      }
    })
  } catch (restoreErr) {
    console.error(`failed to recover ${args.branch} after commit error:`, restoreErr)
    throw new Error(
      `createCommitOnBranch failed and could not restore ${args.branch}; branch may be stuck at base`,
      { cause: restoreErr },
    )
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
    body,
  ]
  if (args.label) createArgs.push('--label', args.label)
  await retry(3, '1s', async () => {
    // First create may have succeeded before a timeout; don't re-issue.
    if (await hasOpenPr(args.branch)) return
    await $`gh ${createArgs}`
  })
}
