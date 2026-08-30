import { ChevronRight } from 'lucide-react'
import type { ReactNode } from 'react'

export function SettingsDisclosure({icon,title,meta,children,className=''}:{icon:ReactNode;title:string;meta?:ReactNode;children:ReactNode;className?:string}){
	return <details className={`settings-disclosure panel ${className}`.trim()}><summary><span className="settings-disclosure-icon">{icon}</span><b>{title}</b>{meta&&<em>{meta}</em>}<ChevronRight size={16}/></summary><div className="settings-disclosure-body">{children}</div></details>
}