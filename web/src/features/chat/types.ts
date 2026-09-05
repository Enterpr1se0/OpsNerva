import { type StreamText } from '../../api/streamText'
import type { ChatTokenUsage } from '../../types'

type ChatEntryImage = {id:string;name:string;mimeType:string;sizeBytes:number;url?:string}
export type PendingChatImage = {id:string;file:File;url:string}
export type ChatEntry = { id: string; sourceMessageId?:string; persisted?:boolean; kind: 'user' | 'assistant' | 'tool' | 'reasoning' | 'error'; content: string; streamText?:StreamText; contentTruncated?:boolean; contentChars?:number; tool?: string; toolCallId?:string; runId?:string; transient?:boolean; progress?:boolean; startedAt?:number; liveStdout?:string; liveStderr?:string; liveOutput?:string; liveOutputStream?:'stdout'|'stderr'; transferredBytes?:number; transferTotalBytes?:number; images?:ChatEntryImage[]; tokenUsage?:ChatTokenUsage; active?: boolean; lifecycle?:'streaming'|'committed'; status?: 'pending' | 'waiting_for_approval' | 'completed' | 'failed' }
export type TaskToolEntryGroup={kind:'task_tool_group';id:string;tool:'TaskCreate'|'TaskUpdate';entries:ChatEntry[]}
export type ChatRenderItem={kind:'entry';entry:ChatEntry}|TaskToolEntryGroup
export type ModelRetryState = {attempt:number;max:number}
export type ConnectionRetryState = {attempt:number;readyAt:number}
export type ContextUsage = {tokens:number;window:number}
