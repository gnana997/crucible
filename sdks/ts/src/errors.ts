// Structured errors, mirroring the Go SDK: every daemon error carries the
// HTTP status and the daemon's {"error": …} text; common statuses get a
// dedicated subclass so callers can `instanceof` instead of matching codes.

export class CrucibleError extends Error {
  /** HTTP status code. */
  readonly status: number;
  /** Reserved machine-readable code (empty today). */
  readonly code: string;

  constructor(status: number, message: string, code = "") {
    super(`daemon returned ${status}: ${message}`);
    this.name = "CrucibleError";
    this.status = status;
    this.code = code;
  }
}

export class NotFoundError extends CrucibleError {
  constructor(message: string) {
    super(404, message);
    this.name = "NotFoundError";
  }
}

export class UnauthorizedError extends CrucibleError {
  constructor(message: string) {
    super(401, message);
    this.name = "UnauthorizedError";
  }
}

export class PolicyDeniedError extends CrucibleError {
  constructor(message: string) {
    super(403, message);
    this.name = "PolicyDeniedError";
  }
}

/** errorFrom maps a non-2xx response onto the typed hierarchy. */
export function errorFrom(status: number, message: string): CrucibleError {
  switch (status) {
    case 404:
      return new NotFoundError(message);
    case 401:
      return new UnauthorizedError(message);
    case 403:
      return new PolicyDeniedError(message);
    default:
      return new CrucibleError(status, message);
  }
}
