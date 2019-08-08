import { connect } from 'react-redux';
import { initSettings, toggleSetting, getLogsInfo, setLogsConfig } from '../actions';
import { getBlockedServices, setBlockedServices } from '../actions/services';
import Settings from '../components/Settings';

const mapStateToProps = (state) => {
    const { settings, services, queryLogs } = state;
    const props = {
        settings,
        services,
        queryLogs,
    };
    return props;
};

const mapDispatchToProps = {
    initSettings,
    toggleSetting,
    getBlockedServices,
    setBlockedServices,
    getLogsInfo,
    setLogsConfig,
};

export default connect(
    mapStateToProps,
    mapDispatchToProps,
)(Settings);
