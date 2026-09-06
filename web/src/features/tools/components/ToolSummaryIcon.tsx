import { Activity, BookOpen, Braces, FileText, FolderOpen, FunctionSquare, ListChecks, Search, Server, TerminalSquare } from 'lucide-react'

export function ToolSummaryIcon({name}:{name:string|undefined}){
	if(name?.startsWith('Task'))return <ListChecks size={15}/>
	if(name==='ssh_task')return <Activity size={15}/>
	if(name?.startsWith('workspace_'))return <FolderOpen size={15}/>
	if(name?.startsWith('web_'))return <Search size={15}/>
	if(name==='skill')return <BookOpen size={15}/>
	if(name?.startsWith('mcp__'))return <Braces size={15}/>
	if(name?.includes('file_'))return <FileText size={15}/>
	if(name?.startsWith('ssh_'))return name==='ssh_exec'||name==='ssh_run_script'||name==='ssh_shell'?<TerminalSquare size={15}/>:<Server size={15}/>
	return <FunctionSquare size={15}/>
}
