export class DockerfileStepsBuilder {
  private readonly steps: string[] = [];

  run(command: string | string[]): this {
    const commands = Array.isArray(command) ? command : [command];
    for (const cmd of commands) {
      this.steps.push(`RUN ${cmd}`);
    }
    return this;
  }

  build(): string[] {
    return [...this.steps];
  }
}
