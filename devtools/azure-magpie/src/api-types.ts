// Payload shapes of /api/changes and /api/changes/<id>. The worker builds them and the UI reads them;
// this module holds types only, so both sides import it without pulling in each other's code.
import type { PatchFile } from "./interdiff";

export interface PullRequestInfo {
  number: number;
  title: string;
  url: string;
  state: "open" | "closed";
  merged: boolean;
}

export interface ChangeListItem {
  id: string;
  subject: string;
  patchsetCount: number;
  updatedAt: string;
  author: { name: string | null; login: string | null };
  review: PullRequestInfo | null;
}

export interface ChangeListPayload {
  repo: string;
  changes: ChangeListItem[];
  truncated: boolean;
  omittedChanges: number;
  note: string | null;
  commitLookupLimit: number;
  reviewUnavailable: string | null;
}

export interface PatchsetInfo {
  number: number;
  sha: string;
  ref: string;
  rootSha: string | null;
  date: string;
  subject: string;
  message: string;
  author: { name: string | null; email: string | null; date: string | null };
  files: PatchFile[] | null;
  filesTruncated: boolean;
  error: string | null;
  // Patchsets (by number, later than this one) whose patch is identical: an estimate of approval carry-over.
  carriesTo: number[];
}

export interface ReviewRecord {
  login: string;
  state: string;
  commitId: string;
  submittedAt: string | null;
}

export interface ApprovalInfo {
  login: string;
  commitId: string;
  patchset: number | null;
  submittedAt: string | null;
}

export interface ChangeDetailPayload {
  repo: string;
  id: string;
  subject: string;
  updatedAt: string;
  patchsets: PatchsetInfo[];
  review: PullRequestInfo | null;
  reviews: ReviewRecord[];
  approvals: ApprovalInfo[];
  reviewUnavailable: string | null;
}
