import { useState } from 'react'
import { useTranslation } from 'react-i18next'
import { ChevronRight, Cpu, Cable, Server, SlidersHorizontal } from 'lucide-react'
import type { Health, Host, ModelProvider, Proxy, SystemSettings, ToolCapabilities } from '../../types'
import { HostsPage } from './components/HostsPage'
import { ProxiesPage } from './components/ProxiesPage'
import { ModelsPage } from './components/ModelsPage'
import { SystemSettingsPage } from './components/SystemSettingsPage'

type ConfigurationSection = 'models' | 'hosts' | 'proxies' | 'system'

export function ConfigurationPage({hosts,providers,proxies,settings,capabilities,health,refreshModels,refreshHosts,refreshProxies,refreshCapabilities,refreshHealth,onSettingsChanged,onOpenMCPActivity}:{hosts:Host[];providers:ModelProvider[];proxies:Proxy[];settings:SystemSettings|null;capabilities:ToolCapabilities;health:Health|null;refreshModels:()=>Promise<void>;refreshHosts:()=>Promise<void>;refreshProxies:()=>Promise<void>;refreshCapabilities:()=>Promise<void>;refreshHealth:()=>Promise<void>;onSettingsChanged:(settings:SystemSettings)=>void;onOpenMCPActivity:()=>void}) {
  const {t}=useTranslation()
  const [section,setSection]=useState<ConfigurationSection>('models')
  const [showAddresses,setShowAddresses]=useState(false)
  const tabs:[ConfigurationSection,React.ReactNode,string,string][]=[
    ['models',<Cpu size={17}/>, t('config.tabs.models'), t('config.configured',{count:providers.length})],
    ['hosts',<Server size={17}/>, t('config.tabs.hosts'), t('config.registered',{count:hosts.length})],
    ['proxies',<Cable size={17}/>, t('config.tabs.proxies'), t('config.configured',{count:proxies.length})],
    ['system',<SlidersHorizontal size={17}/>, t('config.tabs.system'), t('config.maxIterations',{count:settings?.agent_max_iterations??50})],
	  ]
	  return <div className="configuration-center page-stack">
	    <div className="section-tabs-row"><div className="configuration-tabs" role="tablist" aria-label={t('config.sections')}>{tabs.map(([id,icon,label,meta])=><button type="button" role="tab" aria-selected={section===id} className={section===id?'active':''} onClick={()=>setSection(id)} key={id}>{icon}<span><b>{label}</b><small>{meta}</small></span><ChevronRight size={15}/></button>)}</div></div>
    <div className="configuration-content" role="tabpanel">
	  {section==='models'&&(
		<ModelsPage providers={providers} proxies={proxies} showAddresses={showAddresses} onToggleAddresses={()=>setShowAddresses(value=>!value)} refresh={refreshModels}/>
	  )}
	  {section==='hosts'&&(
		<HostsPage hosts={hosts} proxies={proxies} showAddresses={showAddresses} onToggleAddresses={()=>setShowAddresses(value=>!value)} refresh={refreshHosts}/>
	  )}
	  {section==='proxies'&&(
		<ProxiesPage proxies={proxies} showAddresses={showAddresses} onToggleAddresses={()=>setShowAddresses(value=>!value)} refresh={refreshProxies}/>
	  )}
	  {section==='system'&&<SystemSettingsPage settings={settings} providers={providers} proxies={proxies} capabilities={capabilities} modelStatus={health?.model} refreshModels={refreshModels} refreshHosts={refreshHosts} refreshProxies={refreshProxies} refreshCapabilities={refreshCapabilities} refreshHealth={refreshHealth} onSettingsChanged={onSettingsChanged} onOpenMCPActivity={onOpenMCPActivity}/>}
    </div>
  </div>
}
