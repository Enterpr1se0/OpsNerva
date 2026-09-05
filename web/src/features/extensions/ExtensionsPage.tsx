import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { BookOpen, ChevronRight, FunctionSquare, Zap } from 'lucide-react'
import type { LLMToolCatalog, ManagedSkill, MCPServer } from '../../types'
import { MCPServersPage } from './components/MCPServersPage'
import { LLMToolsPage } from './components/LLMToolsPage'
import { SkillsPage } from './components/SkillsPage'

type ExtensionSection = 'skills' | 'mcp' | 'tools'

export function ExtensionsPage({skills,mcpServers,toolCatalog,refreshSkills,refreshMCPServers,refreshToolCatalog,onToolCatalogChanged}:{skills:ManagedSkill[];mcpServers:MCPServer[];toolCatalog:LLMToolCatalog|null;refreshSkills:()=>Promise<void>;refreshMCPServers:()=>Promise<void>;refreshToolCatalog:()=>Promise<void>;onToolCatalogChanged:(catalog:LLMToolCatalog)=>void}){
	const {t}=useTranslation()
		const [section,setSection]=useState<ExtensionSection>('skills')
		const enabledSkills=skills.filter(skill=>skill.enabled).length
		const readyMCP=mcpServers.filter(server=>server.status==='ready').length
		const tabs:[ExtensionSection,React.ReactNode,string,string][]=[
		['skills',<BookOpen size={17}/>, t('extensions.tabs.skills'), t('extensions.enabledRatio',{enabled:enabledSkills,total:skills.length})],
		['mcp',<Zap size={17}/>, t('extensions.tabs.mcp'), t('extensions.readyRatio',{ready:readyMCP,total:mcpServers.length})],
		['tools',<FunctionSquare size={17}/>, t('extensions.tabs.tools'), t('extensions.loaded',{count:toolCatalog?.count??0})],
		]
		return <div className="extensions-center page-stack">
			<div className="section-tabs-row"><div className="extension-tabs configuration-tabs" role="tablist" aria-label={t('extensions.sections')}>{tabs.map(([id,icon,label,meta])=><button type="button" role="tab" aria-selected={section===id} className={section===id?'active':''} onClick={()=>setSection(id)} key={id}>{icon}<span><b>{label}</b><small>{meta}</small></span><ChevronRight size={15}/></button>)}</div></div>
		<div className="configuration-content" role="tabpanel">
			{section==='skills'&&<SkillsPage skills={skills} refreshSkills={refreshSkills} refreshToolCatalog={refreshToolCatalog} onToolCatalogChanged={onToolCatalogChanged}/>}
			{section==='mcp'&&<MCPServersPage servers={mcpServers} refreshServers={refreshMCPServers} refreshToolCatalog={refreshToolCatalog}/>}
			{section==='tools'&&<LLMToolsPage catalog={toolCatalog} refresh={refreshToolCatalog} onCatalogChanged={onToolCatalogChanged}/>}
		</div>
	</div>
}
