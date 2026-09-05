import { useTranslation } from 'react-i18next'
import { ChevronLeft, Eye, EyeOff } from 'lucide-react'

export function ConfigurationEditorPage({icon,title,busy,onBack,children}:{icon:React.ReactNode;title:string;busy?:boolean;onBack:()=>void;children:React.ReactNode}){
	const {t}=useTranslation()
	return <div className="configuration-editor-page">
		<button type="button" className="configuration-editor-back" disabled={busy} onClick={onBack}><ChevronLeft size={16}/>{t('config.backToList')}</button>
		<header className="configuration-editor-header panel"><div>{icon}</div><span><small>{t('config.editor')}</small><h2>{title}</h2></span></header>
		{children}
	</div>
}

export function AddressVisibilityButton({visible,onToggle}:{visible:boolean;onToggle:()=>void}){
	const {t}=useTranslation()
	const label=t(visible?'config.hideAddresses':'config.showAddresses')
	return <button type="button" className={`icon-button configuration-address-toggle ${visible?'active':''}`} aria-label={label} title={label} onClick={onToggle}>{visible?<EyeOff size={17}/>:<Eye size={17}/>}</button>
}
