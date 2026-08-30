export type WorkspaceNotice={kind:'success'|'error';text:string}
export type WorkspaceDeleteCandidate={workspaceID:string;path:string;type:'file'|'directory'}