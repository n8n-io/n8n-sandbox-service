/**
 * Builds Dockerfile step lists for sandbox creation requests.
 */
export class DockerfileStepsBuilder {
  private readonly steps: string[] = [];

  /**
   * Appends one or more `RUN` instructions to the generated Dockerfile.
   */
  run(command: string | string[]): this {
    const commands = Array.isArray(command) ? command : [command];
    for (const cmd of commands) {
      this.steps.push(`RUN ${cmd}`);
    }
    return this;
  }

  /**
   * Returns the accumulated Dockerfile steps.
   */
  build(): string[] {
    return [...this.steps];
  }
}
