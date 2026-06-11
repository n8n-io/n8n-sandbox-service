export const RUNNER_TAGS = {
  docker: '@docker-runner',
  firecracker: '@firecracker-runner',
};

export const BOTH_RUNNERS = {
  tag: [RUNNER_TAGS.docker, RUNNER_TAGS.firecracker],
};
