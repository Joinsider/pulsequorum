/** Response of `GET /api/v1/health`. */
export interface Health {
  status: string;
  role: 'voter' | 'probe';
  nodeName: string;
  version: string;
  commit: string;
}
